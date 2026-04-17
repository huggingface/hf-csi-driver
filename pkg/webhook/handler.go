// Package webhook implements a mutating admission webhook that injects
// hf-mount sidecar containers into pods that use HF CSI volumes.
package webhook

import (
	"context"
	"encoding/json"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	// CSIDriverName is the CSI driver name to detect in pod volumes.
	CSIDriverName = "hf.csi.huggingface.co"

	// SidecarContainerName is the name of the injected sidecar container.
	SidecarContainerName = "hf-mount"

	// TmpVolumeName is the emptyDir volume used for fd-passing communication.
	TmpVolumeName = "hf-csi-tmp"

	// TmpVolumeMountPath is the mount path of the tmp volume inside the sidecar.
	TmpVolumeMountPath = "/hf-csi-tmp"

	// volumeAttribute keys recognised by the injector to configure the
	// resource requests/limits of the injected hf-mount sidecar. Values are
	// standard Kubernetes quantity strings (e.g. "2Gi", "500m").
	volumeAttrMemoryLimit   = "memoryLimit"
	volumeAttrMemoryRequest = "memoryRequest"
	volumeAttrCPULimit      = "cpuLimit"
	volumeAttrCPURequest    = "cpuRequest"
)

// sidecarResources holds the already-parsed quantities collected from
// volumeAttributes to apply to the injected hf-mount sidecar container.
// A nil field means "no user override for this field".
type sidecarResources struct {
	MemoryLimit   *resource.Quantity
	MemoryRequest *resource.Quantity
	CPULimit      *resource.Quantity
	CPURequest    *resource.Quantity
}

// mergeMax combines another per-volume hint into r by taking, per field, the
// larger of the two quantities. The sidecar is a single container shared by
// every HF CSI volume in the pod, so conflicting per-volume hints cannot
// both be honoured; picking the max is order-independent and biases toward
// the most generous hint, which is safe for a shared FUSE process.
func (r *sidecarResources) mergeMax(other sidecarResources) {
	r.MemoryLimit = maxQuantity(r.MemoryLimit, other.MemoryLimit)
	r.MemoryRequest = maxQuantity(r.MemoryRequest, other.MemoryRequest)
	r.CPULimit = maxQuantity(r.CPULimit, other.CPULimit)
	r.CPURequest = maxQuantity(r.CPURequest, other.CPURequest)
}

// maxQuantity returns the larger of a and b, treating nil as "not set".
func maxQuantity(a, b *resource.Quantity) *resource.Quantity {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case a.Cmp(*b) < 0:
		return b
	default:
		return a
	}
}

// resourcesFromVolumeAttrs parses each resource field from volumeAttributes
// up front. Invalid quantity strings are dropped (with a klog warning) and
// treated as "not set" — this guarantees that an invalid early-volume hint
// cannot shadow a valid later-volume hint in mergeMax.
func resourcesFromVolumeAttrs(attrs map[string]string) sidecarResources {
	return sidecarResources{
		MemoryLimit:   parseQuantityAttr(volumeAttrMemoryLimit, attrs[volumeAttrMemoryLimit]),
		MemoryRequest: parseQuantityAttr(volumeAttrMemoryRequest, attrs[volumeAttrMemoryRequest]),
		CPULimit:      parseQuantityAttr(volumeAttrCPULimit, attrs[volumeAttrCPULimit]),
		CPURequest:    parseQuantityAttr(volumeAttrCPURequest, attrs[volumeAttrCPURequest]),
	}
}

func parseQuantityAttr(field, raw string) *resource.Quantity {
	if raw == "" {
		return nil
	}
	q, err := resource.ParseQuantity(raw)
	if err != nil {
		klog.Warningf("Webhook: ignoring invalid %s=%q in volumeAttributes: %v", field, raw, err)
		return nil
	}
	return &q
}

// Config holds the webhook configuration.
type Config struct {
	// SidecarImage is the container image for the sidecar mounter.
	SidecarImage string
}

// Injector is a mutating admission webhook handler that injects the hf-mount
// sidecar into pods that reference HF CSI volumes.
type Injector struct {
	Config  Config
	client  client.Reader
	decoder admission.Decoder
}

// NewInjector creates a new sidecar injector webhook handler.
func NewInjector(config Config, client client.Reader, decoder admission.Decoder) *Injector {
	return &Injector{Config: config, client: client, decoder: decoder}
}

// Handle processes admission requests and injects the sidecar if needed.
func (i *Injector) Handle(ctx context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}
	if err := i.decoder.Decode(req, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Only process CREATE operations.
	if req.Operation != "CREATE" {
		return admission.Allowed("not a create")
	}

	// Count HF CSI volumes (inline or PV-backed) and collect resource hints
	// from their volumeAttributes.
	volumeCount, resources := i.scanHFCSIVolumes(ctx, pod, req.Namespace)
	if volumeCount == 0 {
		return admission.Allowed("no HF CSI volumes")
	}

	// Check if sidecar is already injected (idempotency).
	if hasSidecar(pod) {
		return admission.Allowed("sidecar already injected")
	}

	klog.Infof("Injecting sidecar into pod %s/%s", req.Namespace, pod.GenerateName)

	// Inject the sidecar container and shared volume.
	injectSidecar(pod, i.Config, volumeCount, resources)

	marshaledPod, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod)
}

// scanHFCSIVolumes returns the number of HF CSI volumes in the pod
// (inline ephemeral or PV-backed via PVC) and the merged sidecar resource
// hints collected from their volumeAttributes.
func (i *Injector) scanHFCSIVolumes(ctx context.Context, pod *corev1.Pod, namespace string) (int, sidecarResources) {
	count := 0
	var resources sidecarResources
	for _, vol := range pod.Spec.Volumes {
		switch {
		case vol.CSI != nil && vol.CSI.Driver == CSIDriverName:
			count++
			resources.mergeMax(resourcesFromVolumeAttrs(vol.CSI.VolumeAttributes))
		case vol.PersistentVolumeClaim != nil:
			if pv := i.resolvePVFromPVC(ctx, vol.PersistentVolumeClaim.ClaimName, namespace); pv != nil {
				count++
				resources.mergeMax(resourcesFromVolumeAttrs(pv.Spec.CSI.VolumeAttributes))
			}
		}
	}
	return count, resources
}

// resolvePVFromPVC returns the PV backing the given PVC if (and only if) it
// uses the HF CSI driver, otherwise nil.
func (i *Injector) resolvePVFromPVC(ctx context.Context, pvcName, namespace string) *corev1.PersistentVolume {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := i.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: pvcName}, pvc); err != nil {
		klog.V(4).Infof("Webhook: cannot resolve PVC %s/%s: %v", namespace, pvcName, err)
		return nil
	}
	pvName := pvc.Spec.VolumeName
	if pvName == "" {
		return nil
	}
	pv := &corev1.PersistentVolume{}
	if err := i.client.Get(ctx, client.ObjectKey{Name: pvName}, pv); err != nil {
		klog.V(4).Infof("Webhook: cannot resolve PV %s: %v", pvName, err)
		return nil
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != CSIDriverName {
		return nil
	}
	return pv
}

// hasSidecar returns true if the sidecar is already injected.
func hasSidecar(pod *corev1.Pod) bool {
	for _, c := range pod.Spec.InitContainers {
		if c.Name == SidecarContainerName {
			return true
		}
	}
	for _, c := range pod.Spec.Containers {
		if c.Name == SidecarContainerName {
			return true
		}
	}
	return false
}
