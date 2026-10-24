//go:build linux

package driver

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	mount "k8s.io/mount-utils"
)

// Resolve the production path helper into a temporary directory, without
// requiring access to the real kubelet directory or a Kubernetes cluster.
func sidecarTestPaths(t *testing.T) (podUID, volumeID, volumeDir string) {
	t.Helper()
	podUID, err := filepath.Rel(kubeletPodsBase, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	volumeID = "test-volume"
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(volumeID)))[:12]
	return podUID, volumeID, filepath.Join(sidecarEmptyDirPath(podUID), ".volumes", hash)
}

func TestSidecarHealthMissingHandoff(t *testing.T) {
	podUID, volumeID, volumeDir := sidecarTestPaths(t)
	// Reproduce a tmpfs emptyDir covering the driver's earlier files: the
	// shared volume exists, but its per-volume handoff directory is absent.
	if err := os.MkdirAll(sidecarEmptyDirPath(podUID), 0755); err != nil {
		t.Fatal(err)
	}
	if err := checkSidecarHealth(podUID, volumeID); err == nil {
		t.Fatalf("missing handoff directory %s was reported healthy", volumeDir)
	}
}

func TestSidecarHealthControls(t *testing.T) {
	for _, tc := range []struct {
		name, file, content string
		wantError           bool
	}{
		{name: "starting without optional status files"},
		{name: "healthy ready sidecar", file: "ready", content: "ready"},
		{name: "empty error file", file: "error", content: "\n"},
		{name: "sidecar failure", file: "error", content: "mount failed", wantError: true},
		{name: "driver handoff failure", file: "handoff-error", content: "FUSE fd handoff failed", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			podUID, volumeID, volumeDir := sidecarTestPaths(t)
			if err := os.MkdirAll(volumeDir, 0755); err != nil {
				t.Fatal(err)
			}
			if tc.file != "" {
				if err := os.WriteFile(filepath.Join(volumeDir, tc.file), []byte(tc.content), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if err := checkSidecarHealth(podUID, volumeID); (err != nil) != tc.wantError {
				t.Fatalf("health error = %v, wantError %v", err, tc.wantError)
			}
		})
	}
}

func TestSidecarHealthRejectsSymlinks(t *testing.T) {
	for _, name := range []string{"error", "handoff-error"} {
		t.Run(name, func(t *testing.T) {
			podUID, volumeID, volumeDir := sidecarTestPaths(t)
			if err := os.MkdirAll(volumeDir, 0755); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("must not be read"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(volumeDir, name)); err != nil {
				t.Fatal(err)
			}
			err := checkSidecarHealth(podUID, volumeID)
			if err == nil || strings.Contains(err.Error(), "must not be read") {
				t.Fatalf("expected symlink rejection without reading its target, got %v", err)
			}
		})
	}
}

func TestSidecarMountWaitsForTmpfs(t *testing.T) {
	podUID, volumeID, volumeDir := sidecarTestPaths(t)
	if err := os.MkdirAll(sidecarEmptyDirPath(podUID), 0755); err != nil {
		t.Fatal(err)
	}
	err := sidecarMount("repo", "public/test", t.TempDir(), MountOptions{WorkloadPodUID: podUID}, volumeID)
	if err == nil || !strings.Contains(err.Error(), "not mounted as tmpfs yet") {
		t.Fatalf("expected retry before opening /dev/fuse, got %v", err)
	}
	if _, err := os.Stat(volumeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("handoff directory must not be created before tmpfs: %v", err)
	}
}

func TestRequireSidecarTmpfsExactMount(t *testing.T) {
	infos, err := mount.ParseMountInfo("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range infos {
		if info.FsType != "tmpfs" {
			continue
		}
		tmpfs := unescapeMountPath(info.MountPoint)
		if err := requireSidecarTmpfs(tmpfs); err != nil {
			t.Fatal(err)
		}
		if err := requireSidecarTmpfs(filepath.Join(tmpfs, "not-a-mount")); err == nil {
			t.Fatal("a tmpfs ancestor is not enough to prove emptyDir readiness")
		}
		return
	}
	t.Skip("no tmpfs mount in this test environment")
}

func TestSidecarMountStateRejectsForeignMount(t *testing.T) {
	if _, _, err := sidecarMountState("/proc"); err == nil {
		t.Fatal("must reject foreign mounts rather than replace them")
	}
	if _, mounted, err := sidecarMountState(t.TempDir()); err != nil || mounted {
		t.Fatalf("ordinary directory: mounted=%v, err=%v", mounted, err)
	}
}

func TestNodePublishSidecarMountInspectionFailure(t *testing.T) {
	d, mock, unmounts := newSidecarTestDriver(t)
	d.sidecarMountStateFn = func(string) (int, bool, error) {
		return 0, false, errors.New("cannot read mountinfo")
	}
	_, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: "test-volume", TargetPath: t.TempDir(),
		VolumeCapability: &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}},
		VolumeContext:    map[string]string{volumeCtxSourceType: "repo", volumeCtxSourceID: "public/test", volumeCtxPodUID: "test-pod"},
	})
	if status.Code(err) != codes.Internal || *unmounts != 0 || mock.isMountPointCalls != 0 {
		t.Fatalf("inspection failure must stop publish without touching the mount: err=%v, unmounts=%d, stats=%d", err, *unmounts, mock.isMountPointCalls)
	}
}

func TestNodePublishSidecarRecovery(t *testing.T) {
	for _, tc := range []struct {
		name          string
		healthy       bool
		pending       bool
		handoffFailed bool
		unmountFails  bool
	}{
		{name: "missing handoff"},
		{name: "keep tracking on failed cleanup", unmountFails: true},
		{name: "healthy mount", healthy: true},
		{name: "pending handoff", pending: true},
		{name: "completed failed handoff", healthy: true, handoffFailed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			podUID, volumeID, volumeDir := sidecarTestPaths(t)
			if tc.healthy {
				if err := os.MkdirAll(volumeDir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.handoffFailed {
				if err := os.WriteFile(filepath.Join(volumeDir, "handoff-error"), []byte("FUSE fd handoff failed"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			d, mock, unmounts := newSidecarTestDriver(t)
			target := t.TempDir()
			defer sidecarVolumes.Delete(target)
			const minor = 1234567
			d.sidecarMountStateFn = func(string) (int, bool, error) { return minor, true, nil }
			if tc.pending {
				markSidecarHandoffPending(minor)
				defer clearSidecarHandoffPending(minor)
			}
			if tc.unmountFails {
				d.fuseUnmountFn = func(string) error { *unmounts++; return errors.New("unmount failed") }
			}
			_, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
				VolumeId: volumeID, TargetPath: target,
				VolumeCapability: &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}},
				VolumeContext:    map[string]string{volumeCtxSourceType: "repo", volumeCtxSourceID: "public/test", volumeCtxPodUID: podUID},
			})
			wantRecovery := !tc.pending && (!tc.healthy || tc.handoffFailed)
			if wantRecovery {
				if status.Code(err) != codes.Internal || *unmounts != 1 {
					t.Fatalf("expected retry and nonblocking cleanup, got err=%v, unmounts=%d", err, *unmounts)
				}
			} else if err != nil || *unmounts != 0 {
				t.Fatalf("healthy/pending mount must be preserved: err=%v, unmounts=%d", err, *unmounts)
			}
			if mock.isMountPointCalls != 0 {
				t.Fatal("sidecar republish must not stat the FUSE target")
			}
			if _, tracked := sidecarVolumes.Load(target); tracked != (!wantRecovery || tc.unmountFails) {
				t.Fatalf("unexpected sidecar tracking after cleanup: %v", tracked)
			}
		})
	}
}
