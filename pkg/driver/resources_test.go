package driver

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
)

func TestMountResources_RoundTrip(t *testing.T) {
	orig := ParseMountResources(map[string]string{
		"cpuLimit":      "2",
		"cpuRequest":    "500m",
		"memoryLimit":   "1Gi",
		"memoryRequest": "128Mi",
		"other":         "ignored",
	})

	raw := make(map[string]interface{})
	for k, v := range orig.ToStringMap() {
		raw[k] = v
	}
	got := MountResourcesFromUnstructured(raw)

	check := func(name string, a, b *resource.Quantity) {
		t.Helper()
		if (a == nil) != (b == nil) {
			t.Fatalf("%s nil mismatch: a=%v b=%v", name, a, b)
		}
		if a != nil && a.Cmp(*b) != 0 {
			t.Fatalf("%s: want %s, got %s", name, a.String(), b.String())
		}
	}
	check("CPULimit", orig.CPULimit, got.CPULimit)
	check("CPURequest", orig.CPURequest, got.CPURequest)
	check("MemoryLimit", orig.MemoryLimit, got.MemoryLimit)
	check("MemoryRequest", orig.MemoryRequest, got.MemoryRequest)
}

func TestMountResources_EmptyToStringMap(t *testing.T) {
	if m := (MountResources{}).ToStringMap(); len(m) != 0 {
		t.Fatalf("empty MountResources should serialize to empty map, got %v", m)
	}
}

func TestMountResourcesFromUnstructured_IgnoresNonStrings(t *testing.T) {
	raw := map[string]interface{}{
		"cpuLimit":    "2",
		"cpuRequest":  123, // not a string
		"memoryLimit": nil,
	}
	got := MountResourcesFromUnstructured(raw)
	if got.CPULimit == nil || got.CPULimit.String() != "2" {
		t.Fatalf("CPULimit: want 2, got %v", got.CPULimit)
	}
	if got.CPURequest != nil {
		t.Fatalf("CPURequest: non-string should be dropped, got %v", got.CPURequest)
	}
	if got.MemoryLimit != nil {
		t.Fatalf("MemoryLimit: nil should be dropped, got %v", got.MemoryLimit)
	}
}
