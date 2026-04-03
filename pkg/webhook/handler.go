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
)

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

	// Count HF CSI volumes (inline or PV-backed).
	volumeCount := i.countHFCSIVolumes(ctx, pod, req.Namespace)
	if volumeCount == 0 {
		return admission.Allowed("no HF CSI volumes")
	}

	// Check if sidecar is already injected (idempotency).
	if hasSidecar(pod) {
		return admission.Allowed("sidecar already injected")
	}

	klog.Infof("Injecting sidecar into pod %s/%s", req.Namespace, pod.GenerateName)

	// Inject the sidecar container and shared volume.
	injectSidecar(pod, i.Config, volumeCount)

	marshaledPod, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod)
}

// countHFCSIVolumes returns the number of HF CSI volumes in the pod,
// including both inline ephemeral and PV-backed via PVC.
func (i *Injector) countHFCSIVolumes(ctx context.Context, pod *corev1.Pod, namespace string) int {
	count := 0
	for _, vol := range pod.Spec.Volumes {
		if vol.CSI != nil && vol.CSI.Driver == CSIDriverName {
			count++
		} else if vol.PersistentVolumeClaim != nil {
			if i.isPVCBackedByHFCSI(ctx, vol.PersistentVolumeClaim.ClaimName, namespace) {
				count++
			}
		}
	}
	return count
}

// isPVCBackedByHFCSI resolves a PVC to its PV and checks if the PV uses the HF CSI driver.
func (i *Injector) isPVCBackedByHFCSI(ctx context.Context, pvcName, namespace string) bool {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := i.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: pvcName}, pvc); err != nil {
		klog.V(4).Infof("Webhook: cannot resolve PVC %s/%s: %v", namespace, pvcName, err)
		return false
	}
	pvName := pvc.Spec.VolumeName
	if pvName == "" {
		return false
	}
	pv := &corev1.PersistentVolume{}
	if err := i.client.Get(ctx, client.ObjectKey{Name: pvName}, pv); err != nil {
		klog.V(4).Infof("Webhook: cannot resolve PV %s: %v", pvName, err)
		return false
	}
	return pv.Spec.CSI != nil && pv.Spec.CSI.Driver == CSIDriverName
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
