package webhook

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

var (
	defaultSidecarCPURequest    = resource.MustParse("10m")
	defaultSidecarMemoryRequest = resource.MustParse("32Mi")
)

// buildSidecarResources produces the ResourceRequirements for the injected
// hf-mount container, applying the already-parsed user overrides on top of
// the built-in defaults. If a user override results in request > limit for a
// given resource (e.g. default 32Mi request + memoryLimit: 16Mi), the
// request is clamped down to the limit so the apiserver never rejects the
// pod because of a conflicting per-volume hint.
func buildSidecarResources(overrides sidecarResources) corev1.ResourceRequirements {
	req := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    defaultSidecarCPURequest,
			corev1.ResourceMemory: defaultSidecarMemoryRequest,
		},
	}

	if overrides.CPURequest != nil {
		req.Requests[corev1.ResourceCPU] = *overrides.CPURequest
	}
	if overrides.MemoryRequest != nil {
		req.Requests[corev1.ResourceMemory] = *overrides.MemoryRequest
	}
	if overrides.CPULimit != nil {
		if req.Limits == nil {
			req.Limits = corev1.ResourceList{}
		}
		req.Limits[corev1.ResourceCPU] = *overrides.CPULimit
	}
	if overrides.MemoryLimit != nil {
		if req.Limits == nil {
			req.Limits = corev1.ResourceList{}
		}
		req.Limits[corev1.ResourceMemory] = *overrides.MemoryLimit
	}

	clampRequestToLimit(&req, corev1.ResourceCPU)
	clampRequestToLimit(&req, corev1.ResourceMemory)

	return req
}

// clampRequestToLimit ensures req.Requests[name] <= req.Limits[name].
// Violations are clamped (not rejected) so a typo or an accidentally-small
// limit in volumeAttributes never blocks pod admission.
func clampRequestToLimit(req *corev1.ResourceRequirements, name corev1.ResourceName) {
	if req.Limits == nil {
		return
	}
	lim, hasLim := req.Limits[name]
	if !hasLim {
		return
	}
	rq, hasRq := req.Requests[name]
	if !hasRq {
		return
	}
	if rq.Cmp(lim) > 0 {
		klog.Warningf("Webhook: %s request %s exceeds limit %s; clamping request to limit", name, rq.String(), lim.String())
		req.Requests[name] = lim
	}
}

// injectSidecar adds the hf-mount native sidecar container and the shared
// emptyDir volume to the pod spec. The sidecar runs unprivileged: it receives
// the FUSE fd from the CSI driver via SCM_RIGHTS (the CSI driver does the
// kernel mount).
//
// resources carries optional per-pod resource overrides collected from
// volumeAttributes. Invalid quantity strings are logged and dropped so a
// typo never blocks pod admission.
func injectSidecar(pod *corev1.Pod, config Config, volumeCount int, resources sidecarResources) {
	// Add the shared emptyDir volume for config + socket communication.
	// Use tmpfs (Memory) because the args file may contain the HF token.
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: TmpVolumeName,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{
				Medium: corev1.StorageMediumMemory,
			},
		},
	})

	// Add /tmp as writable emptyDir for sidecar temp files (needed for hf-mount-fuse).
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: "sidecar-tmp",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	})

	// Build the native sidecar container (init container with restartPolicy: Always).
	// Unprivileged: receives fd from CSI driver, does NOT open /dev/fuse.
	sidecar := corev1.Container{
		Name:            SidecarContainerName,
		Image:           config.SidecarImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		RestartPolicy:   ptr.To(corev1.ContainerRestartPolicyAlways),
		Command:         []string{"hf-mount-fuse-sidecar"},
		Args:            []string{"--tmp-dir=" + TmpVolumeMountPath, fmt.Sprintf("--expected-mounts=%d", volumeCount)},
		Env: []corev1.EnvVar{
			{Name: "HOME", Value: "/tmp"},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             ptr.To(true),
			RunAsUser:                ptr.To(int64(65534)),
			RunAsGroup:               ptr.To(int64(65534)),
			ReadOnlyRootFilesystem:   ptr.To(true),
			AllowPrivilegeEscalation: ptr.To(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      TmpVolumeName,
				MountPath: TmpVolumeMountPath,
			},
			{
				Name:      "sidecar-tmp",
				MountPath: "/tmp",
			},
		},
		StartupProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"test", "-f", TmpVolumeMountPath + "/.ready"},
				},
			},
			PeriodSeconds:    1,
			FailureThreshold: 120,
		},
		Resources: buildSidecarResources(resources),
	}

	// Prepend as init container (native sidecar, KEP-753).
	// Must be first so the FUSE daemon is running before other init containers
	// that might access the HF volume.
	pod.Spec.InitContainers = append([]corev1.Container{sidecar}, pod.Spec.InitContainers...)
}
