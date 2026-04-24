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

func (m *mockMounter) CheckHealth(_ string) error {
	return nil
}

func (m *mockMounter) Recover() error {
	return nil
}

func (m *mockMounter) Start(_ <-chan struct{}) {}

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
		{"invalid mountMode", &csi.NodePublishVolumeRequest{
			VolumeId:   "vol1",
			TargetPath: "/mnt",
			VolumeCapability: &csi.VolumeCapability{
				AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			},
			VolumeContext: map[string]string{"sourceType": "bucket", "sourceId": "user/b", "mountMode": "bogus"},
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
	if mock.lastOpts.TokenFile == "" {
		t.Error("expected TokenFile to be set")
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

func TestNodePublishVolume_NoToken(t *testing.T) {
	mock := newMockMounter()
	d := &Driver{mounter: mock, cacheBase: t.TempDir()}
	target := filepath.Join(t.TempDir(), "target")

	// No secrets: public repo, should succeed without token file.
	resp, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
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
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if mock.lastOpts.TokenFile != "" {
		t.Errorf("expected no TokenFile for public repo, got %q", mock.lastOpts.TokenFile)
	}
}

func TestNodePublishVolume_CustomTokenKey(t *testing.T) {
	mock := newMockMounter()
	d := &Driver{mounter: mock, cacheBase: t.TempDir()}
	target := filepath.Join(t.TempDir(), "target")

	resp, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
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
			"tokenKey":   "my-custom-key",
		},
		Secrets: map[string]string{"my-custom-key": "custom-token"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if mock.lastOpts.TokenFile == "" {
		t.Error("expected TokenFile to be set for custom token key")
	}
}

func TestNodePublishVolume_MountFlagsFromVolumeAttributes(t *testing.T) {
	tests := []struct {
		name     string
		flags    string
		expected []string
	}{
		{"single flag", "advanced-writes", []string{"--advanced-writes"}},
		{"multiple flags", "advanced-writes,uid=1000,gid=1000", []string{"--advanced-writes", "--uid=1000", "--gid=1000"}},
		{"whitespace trimmed", "advanced-writes, uid=1000", []string{"--advanced-writes", "--uid=1000"}},
		{"trailing comma ignored", "advanced-writes,", []string{"--advanced-writes"}},
		{"empty string", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := newMockMounter()
			d := &Driver{mounter: mock, cacheBase: t.TempDir()}
			target := filepath.Join(t.TempDir(), "target")

			resp, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
				VolumeId:   "vol1",
				TargetPath: target,
				VolumeCapability: &csi.VolumeCapability{
					AccessType: &csi.VolumeCapability_Mount{
						Mount: &csi.VolumeCapability_MountVolume{},
					},
				},
				VolumeContext: map[string]string{
					"sourceType": "bucket",
					"sourceId":   "user/my-bucket",
					"mountFlags": tt.flags,
				},
				Secrets: map[string]string{"token": "test-token"},
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp == nil {
				t.Fatal("expected non-nil response")
			}
			if len(mock.lastOpts.ExtraArgs) != len(tt.expected) {
				t.Fatalf("expected ExtraArgs %v, got %v", tt.expected, mock.lastOpts.ExtraArgs)
			}
			for i, a := range mock.lastOpts.ExtraArgs {
				if a != tt.expected[i] {
					t.Errorf("ExtraArgs[%d]: expected %q, got %q", i, tt.expected[i], a)
				}
			}
		})
	}
}

func TestNodePublishVolume_VolumeMountGroup(t *testing.T) {
	mock := newMockMounter()
	d := &Driver{mounter: mock, cacheBase: t.TempDir()}
	target := filepath.Join(t.TempDir(), "target")

	resp, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:   "vol1",
		TargetPath: target,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{
					VolumeMountGroup: "1000",
				},
			},
		},
		VolumeContext: map[string]string{
			"sourceType": "bucket",
			"sourceId":   "user/my-bucket",
			"mountFlags": "advanced-writes",
		},
		Secrets: map[string]string{"token": "test-token"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if mock.lastOpts.VolumeMountGroup != "1000" {
		t.Errorf("expected VolumeMountGroup %q, got %q", "1000", mock.lastOpts.VolumeMountGroup)
	}
	// --uid and --gid should be prepended before volumeAttributes mount flags.
	expectedExtra := []string{"--uid=1000", "--gid=1000", "--advanced-writes"}
	if len(mock.lastOpts.ExtraArgs) != len(expectedExtra) {
		t.Fatalf("expected ExtraArgs %v, got %v", expectedExtra, mock.lastOpts.ExtraArgs)
	}
	for i, a := range mock.lastOpts.ExtraArgs {
		if a != expectedExtra[i] {
			t.Errorf("ExtraArgs[%d]: expected %q, got %q", i, expectedExtra[i], a)
		}
	}
}

func TestNodePublishVolume_NoVolumeMountGroup(t *testing.T) {
	mock := newMockMounter()
	d := &Driver{mounter: mock, cacheBase: t.TempDir()}
	target := filepath.Join(t.TempDir(), "target")

	_, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
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
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mock.lastOpts.VolumeMountGroup != "" {
		t.Errorf("expected empty VolumeMountGroup, got %q", mock.lastOpts.VolumeMountGroup)
	}
	// No --uid/--gid should be added when VolumeMountGroup is empty.
	if len(mock.lastOpts.ExtraArgs) != 0 {
		t.Errorf("expected no ExtraArgs, got %v", mock.lastOpts.ExtraArgs)
	}
}

func TestNodeGetCapabilities_VolumeMountGroup(t *testing.T) {
	d := &Driver{}
	resp, err := d.NodeGetCapabilities(context.Background(), &csi.NodeGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, cap := range resp.GetCapabilities() {
		if rpc := cap.GetRpc(); rpc != nil && rpc.GetType() == csi.NodeServiceCapability_RPC_VOLUME_MOUNT_GROUP {
			found = true
		}
	}
	if !found {
		t.Error("expected VOLUME_MOUNT_GROUP capability to be advertised")
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
