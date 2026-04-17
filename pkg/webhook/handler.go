// Package webhook implements a mutating admission webhook that injects
// hf-mount sidecar containers into pods that use HF CSI volumes.
package webhook

import (
	"context"
	"encoding/json"
	"net/http"

	corev1 "k8s.io/api/core/v1"
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

// sidecarResources holds the raw quantity strings read from volumeAttributes
// that will be applied to the injected hf-mount sidecar container.
// Empty fields mean "leave the built-in default unchanged".
type sidecarResources struct {
	MemoryLimit   string
	MemoryRequest string
	CPULimit      string
	CPURequest    string
}

// merge keeps the first non-empty value per field (volumes earlier in
// pod.Spec.Volumes win). The sidecar is a single container shared by all
// HF CSI volumes in the pod, so we cannot honour conflicting per-volume
// hints — document this and pick a deterministic winner.
func (r *sidecarResources) merge(other sidecarResources) {
	if r.MemoryLimit == "" {
		r.MemoryLimit = other.MemoryLimit
	}
	if r.MemoryRequest == "" {
		r.MemoryRequest = other.MemoryRequest
	}
	if r.CPULimit == "" {
		r.CPULimit = other.CPULimit
	}
	if r.CPURequest == "" {
		r.CPURequest = other.CPURequest
	}
}

func resourcesFromVolumeAttrs(attrs map[string]string) sidecarResources {
	return sidecarResources{
		MemoryLimit:   attrs[volumeAttrMemoryLimit],
		MemoryRequest: attrs[volumeAttrMemoryRequest],
		CPULimit:      attrs[volumeAttrCPULimit],
		CPURequest:    attrs[volumeAttrCPURequest],
	}
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
			resources.merge(resourcesFromVolumeAttrs(vol.CSI.VolumeAttributes))
		case vol.PersistentVolumeClaim != nil:
			if pv := i.resolvePVFromPVC(ctx, vol.PersistentVolumeClaim.ClaimName, namespace); pv != nil {
				count++
				resources.merge(resourcesFromVolumeAttrs(pv.Spec.CSI.VolumeAttributes))
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
