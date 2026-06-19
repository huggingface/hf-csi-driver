package driver

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseFuseMounts(t *testing.T) {
	const mountinfo = `22 28 0:21 / /sys rw,nosuid - sysfs sysfs rw
600 30 0:142 / /var/lib/hf-csi-driver/mnt/abc123 rw,nosuid shared:300 - fuse.hf-mount hf-mount rw,user_id=0
601 30 0:142 / /var/lib/kubelet/pods/uid-a/volumes/kubernetes.io~csi/hf-vol-0/mount rw shared:301 - fuse.hf-mount hf-mount rw
602 30 0:200 / /var/lib/kubelet/pods/uid-b/volumes/kubernetes.io~csi/hf-vol-0/mount rw - fuse sshfs rw
603 30 0:99 / /mnt/ext rw - ext4 /dev/x rw
`
	mounts := parseFuseMounts(strings.NewReader(mountinfo))
	if len(mounts) != 3 {
		t.Fatalf("got %d fuse mounts, want 3: %+v", len(mounts), mounts)
	}
	// First two share minor 142 (a source plus its bind target).
	if mounts[0].minor != 142 || mounts[0].source != "hf-mount" || mounts[0].fstype != "fuse.hf-mount" {
		t.Errorf("mount[0] = %+v", mounts[0])
	}
	if mounts[2].minor != 200 || mounts[2].source != "sshfs" {
		t.Errorf("mount[2] = %+v", mounts[2])
	}
}

func TestIsOurFuseMount(t *testing.T) {
	cases := []struct {
		name string
		fm   fuseMount
		want bool
	}{
		{"source mount under mountBaseDir", fuseMount{minor: 1, mountpoint: mountBaseDir + "/abc123", fstype: "fuse.hf-mount", source: "hf-mount"}, true},
		{"sidecar mount by source", fuseMount{minor: 2, mountpoint: "/var/lib/kubelet/pods/uid-a/volumes/kubernetes.io~csi/hf-vol-0/mount", fstype: "fuse", source: "hf-mount"}, true},
		{"foreign fuse at csi path", fuseMount{minor: 3, mountpoint: "/var/lib/kubelet/pods/uid-b/volumes/kubernetes.io~csi/hf-vol-0/mount", fstype: "fuse", source: "sshfs"}, false},
		{"non-fuse", fuseMount{minor: 4, mountpoint: mountBaseDir + "/abc123", fstype: "ext4", source: "hf-mount"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isOurFuseMount(c.fm); got != c.want {
				t.Fatalf("isOurFuseMount(%+v) = %v, want %v", c.fm, got, c.want)
			}
		})
	}
}

func TestVolumeIDForMount(t *testing.T) {
	if v, ok := volumeIDForMount(mountBaseDir + "/abc123def456"); !ok || v != "abc123def456" {
		t.Errorf("source mount: got (%q,%v), want (abc123def456,true)", v, ok)
	}
	target := "/var/lib/kubelet/pods/uid-a/volumes/kubernetes.io~csi/hf-vol-0/mount"
	if v, ok := volumeIDForMount(target); !ok || v != mountID(target) {
		t.Errorf("csi target: got (%q,%v), want (%q,true)", v, ok, mountID(target))
	}
	if _, ok := volumeIDForMount("/some/other/path"); ok {
		t.Error("unrelated path should not yield a volume ID")
	}
}

func TestPodUIDForMount(t *testing.T) {
	uid, ok := podUIDForMount("/var/lib/kubelet/pods/abcd-1234/volumes/kubernetes.io~csi/hf-vol-0/mount")
	if !ok || uid != "abcd-1234" {
		t.Fatalf("got (%q,%v), want (abcd-1234,true)", uid, ok)
	}
	if _, ok := podUIDForMount(mountBaseDir + "/abc123"); ok {
		t.Error("non-kubelet path should not yield a pod UID")
	}
}

func TestCgroupHasPodUID(t *testing.T) {
	// systemd cgroup driver mangles dashes to underscores and adds a pod prefix.
	cg := "0::/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-podabcd_1234.slice/cri-containerd-xyz.scope\n"
	if !cgroupHasPodUID(cg, "abcd-1234") {
		t.Error("expected match for systemd-mangled pod UID")
	}
	if cgroupHasPodUID(cg, "ffff-9999") {
		t.Error("unexpected match for unrelated UID")
	}
	if cgroupHasPodUID(cg, "") {
		t.Error("empty UID must never match")
	}
}

func TestMountHasLiveDaemon(t *testing.T) {
	const vol = "abc123def456"
	srcMount := mountBaseDir + "/" + vol
	sidecarMount := "/var/lib/kubelet/pods/abcd-1234/volumes/kubernetes.io~csi/hf-vol-0/mount"

	t.Run("mount-pod daemon matched by argv", func(t *testing.T) {
		servers := []fuseServerProc{{pid: 10, cmdline: "hf-mount-fuse bucket org/model /mnt/hf/" + vol}}
		if !mountHasLiveDaemon([]string{srcMount}, servers) {
			t.Fatal("expected live daemon match via cmdline")
		}
	})
	t.Run("sidecar daemon matched by cgroup", func(t *testing.T) {
		servers := []fuseServerProc{{pid: 11, cmdline: "hf-mount-fuse-sidecar", cgroup: "0::/kubepods/podabcd_1234/abc"}}
		if !mountHasLiveDaemon([]string{sidecarMount}, servers) {
			t.Fatal("expected live daemon match via cgroup pod UID")
		}
	})
	t.Run("no matching server", func(t *testing.T) {
		servers := []fuseServerProc{{pid: 12, cmdline: "hf-mount-fuse bucket x /mnt/hf/other", cgroup: "0::/kubepods/podffff/abc"}}
		if mountHasLiveDaemon([]string{srcMount, sidecarMount}, servers) {
			t.Fatal("did not expect a live daemon match")
		}
	})
}

// fakeSweep drives the sweeper with in-memory state.
type fakeSweep struct {
	conns      []int
	waiting    map[int]int64
	mounts     map[int][]string
	servers    []fuseServerProc
	connErr    error
	mountsErr  error
	serversErr error
	aborted    []int
}

func (f *fakeSweep) providers() fuseSweepProviders {
	return fuseSweepProviders{
		connections: func() ([]int, error) { return f.conns, f.connErr },
		waiting: func(m int) (int64, error) {
			w, ok := f.waiting[m]
			if !ok {
				return 0, fmt.Errorf("no waiting file for minor %d", m)
			}
			return w, nil
		},
		ourMounts: func() (map[int][]string, error) { return f.mounts, f.mountsErr },
		servers:   func() ([]fuseServerProc, error) { return f.servers, f.serversErr },
		abort:     func(m int) error { f.aborted = append(f.aborted, m); return nil },
	}
}

const testVol = "abc123def456"

func ourMountFor(vol string) map[int][]string {
	return map[int][]string{142: {mountBaseDir + "/" + vol}}
}

func liveServerFor(vol string) []fuseServerProc {
	return []fuseServerProc{{pid: 100, cmdline: "hf-mount-fuse bucket org/m /mnt/hf/" + vol}}
}

func TestSweepAbortsOrphanAfterConfirmWindow(t *testing.T) {
	f := &fakeSweep{
		conns:   []int{142},
		waiting: map[int]int64{142: 1},
		mounts:  ourMountFor(testVol),
		servers: nil, // no daemon serving it -> orphaned
	}
	s := newFuseSweeper(f.providers(), DefaultFuseSweepInterval)

	s.sweep()
	if len(f.aborted) != 0 {
		t.Fatalf("aborted on first sweep, want confirm window: %v", f.aborted)
	}
	s.sweep()
	if len(f.aborted) != 1 || f.aborted[0] != 142 {
		t.Fatalf("expected abort of minor 142 on second sweep, got %v", f.aborted)
	}
	// Already aborted: must not abort again while it lingers.
	s.sweep()
	if len(f.aborted) != 1 {
		t.Fatalf("re-aborted an already-aborted connection: %v", f.aborted)
	}
}

func TestSweepNeverAbortsLiveDaemon(t *testing.T) {
	f := &fakeSweep{
		conns:   []int{142},
		waiting: map[int]int64{142: 5},
		mounts:  ourMountFor(testVol),
		servers: liveServerFor(testVol),
	}
	s := newFuseSweeper(f.providers(), DefaultFuseSweepInterval)
	for i := 0; i < 5; i++ {
		s.sweep()
	}
	if len(f.aborted) != 0 {
		t.Fatalf("aborted a connection with a live daemon: %v", f.aborted)
	}
}

func TestSweepIgnoresForeignAndIdle(t *testing.T) {
	f := &fakeSweep{
		conns:   []int{142, 200},
		waiting: map[int]int64{142: 0, 200: 9}, // 142 idle (ours), 200 busy but foreign
		mounts:  ourMountFor(testVol),          // only 142 is ours
		servers: nil,
	}
	s := newFuseSweeper(f.providers(), DefaultFuseSweepInterval)
	for i := 0; i < 3; i++ {
		s.sweep()
	}
	if len(f.aborted) != 0 {
		t.Fatalf("aborted an idle or foreign connection: %v", f.aborted)
	}
}

func TestSweepResetsStreakOnRecovery(t *testing.T) {
	f := &fakeSweep{
		conns:   []int{142},
		waiting: map[int]int64{142: 1},
		mounts:  ourMountFor(testVol),
		servers: nil,
	}
	s := newFuseSweeper(f.providers(), DefaultFuseSweepInterval)

	s.sweep() // streak 1
	// Daemon comes back before the second confirming sweep.
	f.servers = liveServerFor(testVol)
	s.sweep() // streak reset, no abort
	f.servers = nil
	s.sweep() // streak 1 again
	if len(f.aborted) != 0 {
		t.Fatalf("aborted despite a recovery resetting the streak: %v", f.aborted)
	}
	s.sweep() // streak 2 -> abort
	if len(f.aborted) != 1 {
		t.Fatalf("expected abort after re-accumulating the window, got %v", f.aborted)
	}
}

func TestSweepFailSafeOnProviderErrors(t *testing.T) {
	base := func() *fakeSweep {
		return &fakeSweep{
			conns:   []int{142},
			waiting: map[int]int64{142: 1},
			mounts:  ourMountFor(testVol),
			servers: nil,
		}
	}
	t.Run("servers error", func(t *testing.T) {
		f := base()
		f.serversErr = fmt.Errorf("proc unreadable")
		s := newFuseSweeper(f.providers(), DefaultFuseSweepInterval)
		s.sweep()
		s.sweep()
		if len(f.aborted) != 0 {
			t.Fatalf("aborted despite servers enumeration error: %v", f.aborted)
		}
	})
	t.Run("mounts error", func(t *testing.T) {
		f := base()
		f.mountsErr = fmt.Errorf("mountinfo unreadable")
		s := newFuseSweeper(f.providers(), DefaultFuseSweepInterval)
		s.sweep()
		s.sweep()
		if len(f.aborted) != 0 {
			t.Fatalf("aborted despite mountinfo error: %v", f.aborted)
		}
	})
	t.Run("connections error", func(t *testing.T) {
		f := base()
		f.connErr = fmt.Errorf("sysfs unreadable")
		s := newFuseSweeper(f.providers(), DefaultFuseSweepInterval)
		s.sweep()
		s.sweep()
		if len(f.aborted) != 0 {
			t.Fatalf("aborted despite connections error: %v", f.aborted)
		}
	})
}
