package driver

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
)

func TestSetDefaultMemoryRequests(t *testing.T) {
	origWritable, origReadOnly := DefaultMountMemoryRequest, DefaultMountMemoryRequestReadOnly
	t.Cleanup(func() { DefaultMountMemoryRequest, DefaultMountMemoryRequestReadOnly = origWritable, origReadOnly })

	if err := SetDefaultMemoryRequests("32Mi", "16Mi"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := DefaultMemoryRequestFor(false); got.Cmp(resource.MustParse("32Mi")) != 0 {
		t.Fatalf("writable default: got %s, want 32Mi", got.String())
	}
	if got := DefaultMemoryRequestFor(true); got.Cmp(resource.MustParse("16Mi")) != 0 {
		t.Fatalf("read-only default: got %s, want 16Mi", got.String())
	}

	if err := SetDefaultMemoryRequests("lots", "16Mi"); err == nil {
		t.Fatal("want error for an invalid quantity")
	}
	if got := DefaultMemoryRequestFor(false); got.Cmp(resource.MustParse("32Mi")) != 0 {
		t.Fatalf("invalid input must not change the defaults, got %s", got.String())
	}
}

func TestReadOnlyFromArgs(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"bucket", "org/data", "/mnt/hf/x"}, false},
		{[]string{"--read-only", "bucket", "org/data", "/mnt/hf/x"}, true},
		{[]string{"repo", "org/model", "/mnt/hf/x"}, true},
	}
	for _, tc := range cases {
		if got := ReadOnlyFromArgs(tc.args); got != tc.want {
			t.Errorf("ReadOnlyFromArgs(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}
