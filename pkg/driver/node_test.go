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
	os.Remove(target)
	return nil
}

func (m *mockMounter) IsMountPoint(target string) (bool, error) {
	if _, err := os.Stat(target); os.IsNotExist(err) {
		return false, err
	}
	return m.mounted[target], nil
}

func TestNodePublishVolume_MissingFields(t *testing.T) {
	d := &Driver{mounter: newMockMounter()}

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
		{"missing sourceType", &csi.NodePublishVolumeRequest{
			VolumeId:         "vol1",
			TargetPath:       "/mnt",
			VolumeCapability: &csi.VolumeCapability{},
			VolumeContext:    map[string]string{"sourceId": "user/b"},
		}},
		{"missing sourceId", &csi.NodePublishVolumeRequest{
			VolumeId:         "vol1",
			TargetPath:       "/mnt",
			VolumeCapability: &csi.VolumeCapability{},
			VolumeContext:    map[string]string{"sourceType": "bucket"},
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
	d := &Driver{mounter: mock}

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
	if mock.lastOpts.UID != "1000" {
		t.Errorf("expected UID 1000, got %s", mock.lastOpts.UID)
	}
	if mock.lastOpts.GID != "1000" {
		t.Errorf("expected GID 1000, got %s", mock.lastOpts.GID)
	}
	if !mock.lastOpts.ReadOnly {
		t.Error("expected ReadOnly to be true")
	}
}

func TestNodePublishVolume_Idempotent(t *testing.T) {
	mock := newMockMounter()
	d := &Driver{mounter: mock}

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
	}

	// First call.
	if _, err := d.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Second call should succeed (idempotent).
	if _, err := d.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("second call: %v", err)
	}
}

func TestNodeUnpublishVolume_MissingFields(t *testing.T) {
	d := &Driver{mounter: newMockMounter()}

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
	d := &Driver{mounter: newMockMounter()}

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

func TestNodeUnpublishVolume_Success(t *testing.T) {
	mock := newMockMounter()
	d := &Driver{mounter: mock}

	target := filepath.Join(t.TempDir(), "target")
	os.MkdirAll(target, 0750)
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
