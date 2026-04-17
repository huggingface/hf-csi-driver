package webhook

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

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
		CPURequest:    "250m",
		MemoryRequest: "128Mi",
		CPULimit:      "1",
		MemoryLimit:   "2Gi",
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
// dropped and the default (or previously-merged value) stays in place.
func TestBuildSidecarResources_InvalidQuantityIgnored(t *testing.T) {
	r := buildSidecarResources(sidecarResources{
		MemoryLimit:   "not-a-quantity",
		MemoryRequest: "64Mi",
	})

	if got, want := r.Requests[corev1.ResourceMemory], resource.MustParse("64Mi"); got.Cmp(want) != 0 {
		t.Fatalf("memory request: got %s, want %s", got.String(), want.String())
	}
	if _, ok := r.Limits[corev1.ResourceMemory]; ok {
		t.Fatalf("invalid memory limit should have been dropped, got %v", r.Limits[corev1.ResourceMemory])
	}
}

func TestResourcesMerge_FirstNonEmptyWins(t *testing.T) {
	a := resourcesFromVolumeAttrs(map[string]string{
		"memoryLimit": "2Gi",
		"cpuRequest":  "100m",
	})
	b := resourcesFromVolumeAttrs(map[string]string{
		"memoryLimit":   "4Gi", // should be ignored (a already set it)
		"memoryRequest": "256Mi",
	})

	a.merge(b)

	if a.MemoryLimit != "2Gi" {
		t.Fatalf("memoryLimit: want first-wins 2Gi, got %q", a.MemoryLimit)
	}
	if a.MemoryRequest != "256Mi" {
		t.Fatalf("memoryRequest: want 256Mi from second volume, got %q", a.MemoryRequest)
	}
	if a.CPURequest != "100m" {
		t.Fatalf("cpuRequest: want 100m, got %q", a.CPURequest)
	}
}
