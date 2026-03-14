package driver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

// mockMounter implements Mounter for testing.
type mockMounter struct {
	mounted map[string]bool
	lastOpts MountOptions
}

func newMockMounter() *mockMounter {
	return &mockMounter{mounted: make(map[string]bool)}
}

func (m *mockMounter) Mount(sourceType, sourceID, target string, opts MountOptions) error {
	m.mounted[target] = true
	m.lastOpts = opts
	return nil
}

func (m *mockMounter) Unmount(target string) error {
	delete(m.mounted, target)
	_ = os.Remove(target)
	return nil
}

func (m *mockMounter) IsMountPoint(target string) (bool, error) {
	if _, err := os.Stat(target); os.IsNotExist(err) {
		return false, err
	}
	return m.mounted[target], nil
}

func TestNodePublishVolume_MissingFields(t *testing.T) {
	d := &Driver{mounter: newMockMounter(), cacheBase: t.TempDir()}

	tests := []struct {
		name string
		req  *csi.NodePublishVolumeRequest
	}{
		{"missing volumeId", &csi.NodePublishVolumeRequest{
			TargetPath:       "/mnt",
			VolumeCapability: &csi.VolumeCapability{},
		}},
		{"missing targetPath", &csi.NodePublishVolumeRequest{
			VolumeId:         "vol1",
			VolumeCapability: &csi.VolumeCapability{},
		}},
		{"missing volumeCapability", &csi.NodePublishVolumeRequest{
			VolumeId:   "vol1",
			TargetPath: "/mnt",
		}},
		{"block access type", &csi.NodePublishVolumeRequest{
			VolumeId:         "vol1",
			TargetPath:       "/mnt",
			VolumeCapability: &csi.VolumeCapability{},
			VolumeContext:    map[string]string{"sourceType": "bucket", "sourceId": "user/b"},
		}},
		{"missing sourceType", &csi.NodePublishVolumeRequest{
			VolumeId:   "vol1",
			TargetPath: "/mnt",
			VolumeCapability: &csi.VolumeCapability{
				AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			},
			VolumeContext: map[string]string{"sourceId": "user/b"},
		}},
		{"missing sourceId", &csi.NodePublishVolumeRequest{
			VolumeId:   "vol1",
			TargetPath: "/mnt",
			VolumeCapability: &csi.VolumeCapability{
				AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			},
			VolumeContext: map[string]string{"sourceType": "bucket"},
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := d.NodePublishVolume(context.Background(), tt.req)
			if err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestNodePublishVolume_Success(t *testing.T) {
	mock := newMockMounter()
	d := &Driver{mounter: mock, cacheBase: t.TempDir()}

	target := filepath.Join(t.TempDir(), "target")

	resp, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:   "vol1",
		TargetPath: target,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{
					MountFlags: []string{"uid=1000", "gid=1000", "read-only"},
				},
			},
		},
		VolumeContext: map[string]string{
			"sourceType": "bucket",
			"sourceId":   "user/my-bucket",
		},
		Secrets: map[string]string{"token": "test-token"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if !mock.mounted[target] {
		t.Error("expected target to be mounted")
	}
	// Mount flags are passed through as extra args.
	expectedExtra := []string{"--uid=1000", "--gid=1000", "--read-only"}
	if len(mock.lastOpts.ExtraArgs) != len(expectedExtra) {
		t.Fatalf("expected ExtraArgs %v, got %v", expectedExtra, mock.lastOpts.ExtraArgs)
	}
	for i, a := range mock.lastOpts.ExtraArgs {
		if a != expectedExtra[i] {
			t.Errorf("ExtraArgs[%d]: expected %q, got %q", i, expectedExtra[i], a)
		}
	}
	// Token file should be passed.
	if mock.lastOpts.TokenFile == "" {
		t.Error("expected TokenFile to be set")
	}
}

func TestNodePublishVolume_Idempotent(t *testing.T) {
	mock := newMockMounter()
	d := &Driver{mounter: mock, cacheBase: t.TempDir()}

	target := filepath.Join(t.TempDir(), "target")

	req := &csi.NodePublishVolumeRequest{
		VolumeId:   "vol1",
		TargetPath: target,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{},
			},
		},
		VolumeContext: map[string]string{
			"sourceType": "bucket",
			"sourceId":   "user/b",
		},
		Secrets: map[string]string{"token": "test-token"},
	}

	// First call.
	if _, err := d.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Second call (republish) should succeed and update token file.
	req.Secrets["token"] = "refreshed-token"
	if _, err := d.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("second call: %v", err)
	}
}

func TestNodeUnpublishVolume_MissingFields(t *testing.T) {
	d := &Driver{mounter: newMockMounter(), cacheBase: t.TempDir()}

	tests := []struct {
		name string
		req  *csi.NodeUnpublishVolumeRequest
	}{
		{"missing volumeId", &csi.NodeUnpublishVolumeRequest{TargetPath: "/mnt"}},
		{"missing targetPath", &csi.NodeUnpublishVolumeRequest{VolumeId: "vol1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := d.NodeUnpublishVolume(context.Background(), tt.req)
			if err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestNodeUnpublishVolume_NotMounted(t *testing.T) {
	d := &Driver{mounter: newMockMounter(), cacheBase: t.TempDir()}

	resp, err := d.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "vol1",
		TargetPath: "/nonexistent/path",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
}

func TestSanitizeVolumeID(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"my-volume", "my-volume"},
		{"user/bucket", "user%2Fbucket"},
		{"../../../etc/passwd", "..%2F..%2F..%2Fetc%2Fpasswd"},
		{"/absolute/path", "%2Fabsolute%2Fpath"},
		{"a_b", "a_b"},
		{".", "%2E"},
		{"..", "%2E%2E"},
	}
	for _, tt := range tests {
		got := sanitizeVolumeID(tt.input)
		if got != tt.expected {
			t.Errorf("sanitizeVolumeID(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestNodeUnpublishVolume_Success(t *testing.T) {
	mock := newMockMounter()
	d := &Driver{mounter: mock, cacheBase: t.TempDir()}

	target := filepath.Join(t.TempDir(), "target")
	_ = os.MkdirAll(target, 0750)
	mock.mounted[target] = true

	resp, err := d.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "vol1",
		TargetPath: target,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if mock.mounted[target] {
		t.Error("expected target to be unmounted")
	}
}
