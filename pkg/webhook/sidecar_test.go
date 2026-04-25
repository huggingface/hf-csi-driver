package webhook

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	"github.com/huggingface/hf-buckets-csi-driver/pkg/driver"
)

// quantityPtr is a small helper to get a *resource.Quantity from a literal.
func quantityPtr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

func buildFromOverrides(overrides driver.MountResources) corev1.ResourceRequirements {
	return driver.BuildResourceRequirements(overrides, driver.DefaultMountCPURequest, driver.DefaultMountMemoryRequest)
}

func TestBuildSidecarResources_Defaults(t *testing.T) {
	r := buildFromOverrides(driver.MountResources{})

	if got, want := r.Requests[corev1.ResourceCPU], resource.MustParse("10m"); got.Cmp(want) != 0 {
		t.Fatalf("default cpu request: got %s, want %s", got.String(), want.String())
	}
	if got, want := r.Requests[corev1.ResourceMemory], resource.MustParse("32Mi"); got.Cmp(want) != 0 {
		t.Fatalf("default memory request: got %s, want %s", got.String(), want.String())
	}
	if len(r.Limits) != 0 {
		t.Fatalf("expected no default limits, got %#v", r.Limits)
	}
}

func TestBuildSidecarResources_Overrides(t *testing.T) {
	r := buildFromOverrides(driver.MountResources{
		CPURequest:    quantityPtr("250m"),
		MemoryRequest: quantityPtr("128Mi"),
		CPULimit:      quantityPtr("1"),
		MemoryLimit:   quantityPtr("2Gi"),
	})

	if got, want := r.Requests[corev1.ResourceCPU], resource.MustParse("250m"); got.Cmp(want) != 0 {
		t.Fatalf("cpu request: got %s, want %s", got.String(), want.String())
	}
	if got, want := r.Requests[corev1.ResourceMemory], resource.MustParse("128Mi"); got.Cmp(want) != 0 {
		t.Fatalf("memory request: got %s, want %s", got.String(), want.String())
	}
	if got, want := r.Limits[corev1.ResourceCPU], resource.MustParse("1"); got.Cmp(want) != 0 {
		t.Fatalf("cpu limit: got %s, want %s", got.String(), want.String())
	}
	if got, want := r.Limits[corev1.ResourceMemory], resource.MustParse("2Gi"); got.Cmp(want) != 0 {
		t.Fatalf("memory limit: got %s, want %s", got.String(), want.String())
	}
}

// A typo should not prevent pod admission: invalid quantity strings are
// dropped at parse time (driver.ParseMountResources) and the field stays nil,
// so the builder falls back to the default.
func TestResourcesFromVolumeAttrs_InvalidQuantityDropped(t *testing.T) {
	r := driver.ParseMountResources(map[string]string{
		"memoryLimit":   "not-a-quantity",
		"memoryRequest": "64Mi",
	})

	if r.MemoryLimit != nil {
		t.Fatalf("invalid memoryLimit should have parsed to nil, got %v", r.MemoryLimit)
	}
	if r.MemoryRequest == nil || r.MemoryRequest.String() != "64Mi" {
		t.Fatalf("memoryRequest: want 64Mi, got %v", r.MemoryRequest)
	}
}

// Regression: an invalid string in one volume's volumeAttributes must NOT
// shadow a valid override from another volume. Before the fix, merge ran on
// raw strings and the invalid (but non-empty) first value won, only to be
// dropped at parse time, leaving the sidecar unbounded.
func TestResourcesMergeMax_InvalidDoesNotShadowValid(t *testing.T) {
	a := driver.ParseMountResources(map[string]string{
		"memoryLimit": "not-a-quantity", // parses to nil
	})
	b := driver.ParseMountResources(map[string]string{
		"memoryLimit": "2Gi",
	})

	mergeMaxResources(&a, b)

	if a.MemoryLimit == nil || a.MemoryLimit.Cmp(resource.MustParse("2Gi")) != 0 {
		t.Fatalf("want merged memoryLimit=2Gi, got %v", a.MemoryLimit)
	}
}

// mergeMax must be order-independent and pick the larger value per field.
func TestResourcesMergeMax_TakesMaxAndIsOrderIndependent(t *testing.T) {
	a1 := driver.ParseMountResources(map[string]string{"memoryLimit": "2Gi"})
	b1 := driver.ParseMountResources(map[string]string{"memoryLimit": "4Gi"})
	mergeMaxResources(&a1, b1)

	a2 := driver.ParseMountResources(map[string]string{"memoryLimit": "4Gi"})
	b2 := driver.ParseMountResources(map[string]string{"memoryLimit": "2Gi"})
	mergeMaxResources(&a2, b2)

	want := resource.MustParse("4Gi")
	if a1.MemoryLimit.Cmp(want) != 0 {
		t.Fatalf("a.mergeMax(b): want 4Gi, got %s", a1.MemoryLimit.String())
	}
	if a2.MemoryLimit.Cmp(want) != 0 {
		t.Fatalf("b.mergeMax(a): want 4Gi, got %s", a2.MemoryLimit.String())
	}
}

// Clamp: a user-set limit smaller than the default request must not produce
// request > limit (apiserver would reject the pod). We clamp request down
// to the limit and log a warning.
func TestBuildSidecarResources_RequestClampedToLimit(t *testing.T) {
	// memoryLimit: 16Mi is smaller than the default 32Mi request.
	r := buildFromOverrides(driver.MountResources{
		MemoryLimit: quantityPtr("16Mi"),
	})

	rq := r.Requests[corev1.ResourceMemory]
	lim := r.Limits[corev1.ResourceMemory]
	if rq.Cmp(lim) > 0 {
		t.Fatalf("request %s must not exceed limit %s after clamp", rq.String(), lim.String())
	}
	if rq.Cmp(resource.MustParse("16Mi")) != 0 {
		t.Fatalf("memory request: want clamped to 16Mi, got %s", rq.String())
	}
}

// Clamp also applies when both request and limit are user-supplied but
// request > limit (e.g. mergeMax pulled each from different volumes).
func TestBuildSidecarResources_ClampUserProvidedRequestAboveLimit(t *testing.T) {
	r := buildFromOverrides(driver.MountResources{
		MemoryRequest: quantityPtr("512Mi"),
		MemoryLimit:   quantityPtr("256Mi"),
	})

	rq := r.Requests[corev1.ResourceMemory]
	lim := r.Limits[corev1.ResourceMemory]
	if rq.Cmp(lim) > 0 {
		t.Fatalf("request %s must not exceed limit %s after clamp", rq.String(), lim.String())
	}
	if rq.Cmp(lim) != 0 {
		t.Fatalf("clamped request should equal limit 256Mi, got %s", rq.String())
	}
}

// Nil VolumeAttributes (possible on malformed PVs) must not panic.
func TestResourcesFromVolumeAttrs_NilMap(t *testing.T) {
	r := driver.ParseMountResources(nil)
	if r.MemoryLimit != nil || r.MemoryRequest != nil || r.CPULimit != nil || r.CPURequest != nil {
		t.Fatalf("nil attrs must yield all-nil resources, got %#v", r)
	}
}

// Pods with no explicit grace period get the minimum bumped in so the sidecar
// has time to flush after SIGTERM. The original value ("unset") is recorded
// as an annotation.
func TestEnsureTerminationGracePeriod_Unset(t *testing.T) {
	pod := &corev1.Pod{}
	ensureTerminationGracePeriod(pod)
	if pod.Spec.TerminationGracePeriodSeconds == nil ||
		*pod.Spec.TerminationGracePeriodSeconds != MinTerminationGracePeriodSeconds {
		t.Fatalf("want grace=%d, got %v", MinTerminationGracePeriodSeconds, pod.Spec.TerminationGracePeriodSeconds)
	}
	if got := pod.Annotations[AnnotationOriginalGracePeriod]; got != "unset" {
		t.Fatalf("want annotation %q=unset, got %q", AnnotationOriginalGracePeriod, got)
	}
}

// Jobs pods set grace=0 (immediate SIGKILL); the webhook must raise it,
// otherwise pending writes never make it back to the bucket. The original
// "0" must be preserved in the annotation so the bump is traceable.
func TestEnsureTerminationGracePeriod_ZeroIsRaised(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{TerminationGracePeriodSeconds: ptr.To[int64](0)}}
	ensureTerminationGracePeriod(pod)
	if got := *pod.Spec.TerminationGracePeriodSeconds; got != MinTerminationGracePeriodSeconds {
		t.Fatalf("want grace=%d, got %d", MinTerminationGracePeriodSeconds, got)
	}
	if got := pod.Annotations[AnnotationOriginalGracePeriod]; got != "0" {
		t.Fatalf("want annotation %q=0, got %q", AnnotationOriginalGracePeriod, got)
	}
}

// A grace period below the minimum is raised to the minimum and recorded.
func TestEnsureTerminationGracePeriod_BelowMinIsRaised(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{TerminationGracePeriodSeconds: ptr.To[int64](5)}}
	ensureTerminationGracePeriod(pod)
	if got := *pod.Spec.TerminationGracePeriodSeconds; got != MinTerminationGracePeriodSeconds {
		t.Fatalf("want grace=%d, got %d", MinTerminationGracePeriodSeconds, got)
	}
	if got := pod.Annotations[AnnotationOriginalGracePeriod]; got != "5" {
		t.Fatalf("want annotation %q=5, got %q", AnnotationOriginalGracePeriod, got)
	}
}

// A pod that already requests more grace than the minimum (Endpoints uses
// 3600s) must not be lowered, and no annotation is added.
func TestEnsureTerminationGracePeriod_AboveMinIsKept(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{TerminationGracePeriodSeconds: ptr.To[int64](3600)}}
	ensureTerminationGracePeriod(pod)
	if got := *pod.Spec.TerminationGracePeriodSeconds; got != 3600 {
		t.Fatalf("want grace=3600 preserved, got %d", got)
	}
	if _, ok := pod.Annotations[AnnotationOriginalGracePeriod]; ok {
		t.Fatalf("annotation %q must not be set when grace was already sufficient", AnnotationOriginalGracePeriod)
	}
}

// Exactly equal to the minimum is a no-op (no annotation either).
func TestEnsureTerminationGracePeriod_EqualMinIsKept(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{TerminationGracePeriodSeconds: ptr.To(MinTerminationGracePeriodSeconds)}}
	ensureTerminationGracePeriod(pod)
	if got := *pod.Spec.TerminationGracePeriodSeconds; got != MinTerminationGracePeriodSeconds {
		t.Fatalf("want grace=%d, got %d", MinTerminationGracePeriodSeconds, got)
	}
	if _, ok := pod.Annotations[AnnotationOriginalGracePeriod]; ok {
		t.Fatalf("annotation %q must not be set when grace was already sufficient", AnnotationOriginalGracePeriod)
	}
}

// injectSidecar should set the grace period and record the bump as part of
// the same admission patch.
func TestInjectSidecar_SetsGracePeriodAndAnnotation(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{TerminationGracePeriodSeconds: ptr.To[int64](0)}}
	injectSidecar(pod, Config{SidecarImage: "test:latest"}, 1, driver.MountResources{})
	if pod.Spec.TerminationGracePeriodSeconds == nil ||
		*pod.Spec.TerminationGracePeriodSeconds != MinTerminationGracePeriodSeconds {
		t.Fatalf("want grace=%d after injection, got %v", MinTerminationGracePeriodSeconds, pod.Spec.TerminationGracePeriodSeconds)
	}
	if got := pod.Annotations[AnnotationOriginalGracePeriod]; got != "0" {
		t.Fatalf("want annotation %q=0 after injection, got %q", AnnotationOriginalGracePeriod, got)
	}
}
