package webhook

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// quantityPtr is a small helper to get a *resource.Quantity from a literal.
func quantityPtr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

func TestBuildSidecarResources_Defaults(t *testing.T) {
	r := buildSidecarResources(sidecarResources{})

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
	r := buildSidecarResources(sidecarResources{
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
// dropped at parse time (resourcesFromVolumeAttrs) and the field stays nil,
// so buildSidecarResources falls back to the default.
func TestResourcesFromVolumeAttrs_InvalidQuantityDropped(t *testing.T) {
	r := resourcesFromVolumeAttrs(map[string]string{
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
	a := resourcesFromVolumeAttrs(map[string]string{
		"memoryLimit": "not-a-quantity", // parses to nil
	})
	b := resourcesFromVolumeAttrs(map[string]string{
		"memoryLimit": "2Gi",
	})

	a.mergeMax(b)

	if a.MemoryLimit == nil || a.MemoryLimit.Cmp(resource.MustParse("2Gi")) != 0 {
		t.Fatalf("want merged memoryLimit=2Gi, got %v", a.MemoryLimit)
	}
}

// mergeMax must be order-independent and pick the larger value per field.
func TestResourcesMergeMax_TakesMaxAndIsOrderIndependent(t *testing.T) {
	a1 := resourcesFromVolumeAttrs(map[string]string{"memoryLimit": "2Gi"})
	b1 := resourcesFromVolumeAttrs(map[string]string{"memoryLimit": "4Gi"})
	a1.mergeMax(b1)

	a2 := resourcesFromVolumeAttrs(map[string]string{"memoryLimit": "4Gi"})
	b2 := resourcesFromVolumeAttrs(map[string]string{"memoryLimit": "2Gi"})
	a2.mergeMax(b2)

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
	r := buildSidecarResources(sidecarResources{
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
	r := buildSidecarResources(sidecarResources{
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
	r := resourcesFromVolumeAttrs(nil)
	if r.MemoryLimit != nil || r.MemoryRequest != nil || r.CPULimit != nil || r.CPURequest != nil {
		t.Fatalf("nil attrs must yield all-nil resources, got %#v", r)
	}
}
