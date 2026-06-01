package webhook

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/huggingface/hf-buckets-csi-driver/pkg/driver"
)

// MinTerminationGracePeriodSeconds is the minimum pod-level grace period the
// webhook enforces when a sidecar is injected. The sidecar receives SIGTERM at
// pod shutdown and may need several seconds to flush pending writes back to
// the bucket: the FUSE flush queue uses a 2s debounce and batches up to 30s of
// changes, and the upload itself is bounded by network throughput. Pods with
// a shorter grace period (notably Jobs, which sets it to 0) get SIGKILLed
// before the flush completes and lose data.
const MinTerminationGracePeriodSeconds int64 = 60

// AnnotationOriginalGracePeriod records the pod-level
// terminationGracePeriodSeconds the webhook observed before raising it to
// MinTerminationGracePeriodSeconds. The annotation is only set when the
// webhook actually changed the value, so its presence is a signal that the
// bump happened. The value is the original number as a string ("0", "5",
// ...), or "unset" when the field was nil. Lets operators understand why a
// pod takes longer to terminate than the spec they wrote.
const AnnotationOriginalGracePeriod = "hf.csi.huggingface.co/original-termination-grace-period-seconds"

// injectSidecar adds the hf-mount native sidecar container and the shared
// emptyDir volume to the pod spec. The sidecar runs unprivileged: it receives
// the FUSE fd from the CSI driver via SCM_RIGHTS (the CSI driver does the
// kernel mount).
//
// resources carries optional per-pod resource overrides collected from
// volumeAttributes. Invalid quantity strings are logged and dropped so a
// typo never blocks pod admission.
func injectSidecar(pod *corev1.Pod, config Config, volumeCount int, resources driver.MountResources) {
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
		Env: append([]corev1.EnvVar{
			{Name: "HOME", Value: "/tmp"},
		}, config.SidecarEnv...),
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
		Resources: driver.BuildResourceRequirements(resources, driver.DefaultMountCPURequest, driver.DefaultMountMemoryRequest),
	}

	// Prepend as init container (native sidecar, KEP-753).
	// Must be first so the FUSE daemon is running before other init containers
	// that might access the HF volume.
	pod.Spec.InitContainers = append([]corev1.Container{sidecar}, pod.Spec.InitContainers...)

	ensureTerminationGracePeriod(pod)
}

// ensureTerminationGracePeriod raises the pod-level grace period to at least
// MinTerminationGracePeriodSeconds so the sidecar has time to flush pending
// writes after receiving SIGTERM. Pods that already request a longer grace
// period are left unchanged. When the webhook changes the value it records
// the original in AnnotationOriginalGracePeriod so the bump is traceable.
func ensureTerminationGracePeriod(pod *corev1.Pod) {
	current := pod.Spec.TerminationGracePeriodSeconds
	if current != nil && *current >= MinTerminationGracePeriodSeconds {
		return
	}

	original := "unset"
	if current != nil {
		original = fmt.Sprintf("%d", *current)
	}

	pod.Spec.TerminationGracePeriodSeconds = ptr.To(MinTerminationGracePeriodSeconds)

	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[AnnotationOriginalGracePeriod] = original
}
