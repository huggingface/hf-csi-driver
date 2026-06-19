package driver

import (
	"path/filepath"
	"strings"
	"time"

	"k8s.io/klog/v2"
)

const (
	// DefaultFuseSweepInterval is how often the orphaned-connection sweep runs.
	DefaultFuseSweepInterval = 60 * time.Second

	// fuseMountSource is the mount "source" string our FUSE mounts use (the
	// field after the fstype in mountinfo — see sidecarMount's syscall.Mount).
	// It scopes the sweep strictly to our connections so a foreign FUSE
	// filesystem (another CSI driver, gvisor, sshfs, …) is never aborted.
	fuseMountSource = "hf-mount"

	// kubeletCSIMarker identifies a kubelet CSI publish target path. Used to
	// derive the workload pod UID and the volume ID from a mountpoint.
	kubeletCSIMarker = "/volumes/kubernetes.io~csi/"

	// orphanConfirmSweeps is how many consecutive sweeps a connection must look
	// orphaned before we abort it. `waiting` fluctuates and a daemon can be
	// momentarily busy, so a single observation is unreliable; requiring two
	// sweeps (~one interval) of sustained orphaning avoids racing a live daemon.
	orphanConfirmSweeps = 2
)

// fuseServerProc is a live process holding an open /dev/fuse fd — i.e. a process
// that could be serving a FUSE connection. Zombies never appear here: a defunct
// process has already released its file descriptors, which is precisely the
// signal we rely on to tell a dead daemon from a live one.
type fuseServerProc struct {
	pid     int
	cmdline string // argv, space-joined
	cgroup  string // /proc/<pid>/cgroup contents
}

// fuseSweepProviders abstracts all host I/O so the decision logic is unit
// testable without touching /sys or /proc.
type fuseSweepProviders struct {
	// connections lists the connection minors under /sys/fs/fuse/connections.
	connections func() ([]int, error)
	// waiting reports the `waiting` counter for a connection minor.
	waiting func(minor int) (int64, error)
	// ourMounts maps connection minor -> our FUSE mountpoints, already scoped to
	// this driver. A minor absent from the map is foreign and never aborted.
	ourMounts func() (map[int][]string, error)
	// servers lists live processes holding an open /dev/fuse fd.
	servers func() ([]fuseServerProc, error)
	// abort force-aborts a connection minor.
	abort func(minor int) error
}

// fuseSweeper periodically aborts FUSE connections whose serving daemon is gone
// (zombie/absent) so a wedged mount cannot strand pod teardown — neither its own
// (umount stuck in fuse_kill_sb_anon) nor unrelated pods on the same node (a
// node-wide sync(2) blocks on the dead superblock). It is the recovery path for
// connections already orphaned with no NodeUnpublishVolume in flight, which the
// unpublish-time abort (issue #47) never reaches.
type fuseSweeper struct {
	p        fuseSweepProviders
	interval time.Duration
	// orphanStreak counts consecutive sweeps a minor has looked orphaned.
	orphanStreak map[int]int
	// aborted marks minors already aborted, so we do not re-abort (and re-log)
	// every sweep while the now-errored connection lingers in sysfs.
	aborted map[int]bool
}

func newFuseSweeper(p fuseSweepProviders, interval time.Duration) *fuseSweeper {
	if interval <= 0 {
		interval = DefaultFuseSweepInterval
	}
	return &fuseSweeper{
		p:            p,
		interval:     interval,
		orphanStreak: make(map[int]int),
		aborted:      make(map[int]bool),
	}
}

func (s *fuseSweeper) run(stopCh <-chan struct{}) {
	klog.Infof("FUSE orphan sweeper started (interval=%s)", s.interval)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		s.sweep()
		select {
		case <-stopCh:
			klog.Info("FUSE orphan sweeper stopping")
			return
		case <-t.C:
		}
	}
}

func (s *fuseSweeper) sweep() {
	// Fail safe: if we cannot authoritatively enumerate any input, do nothing.
	// A transient read error must never look like "every daemon is gone" and
	// trigger a mass abort.
	conns, err := s.p.connections()
	if err != nil {
		klog.V(4).Infof("fuse sweeper: list connections: %v", err)
		return
	}
	mounts, err := s.p.ourMounts()
	if err != nil {
		klog.V(4).Infof("fuse sweeper: list mounts: %v", err)
		return
	}
	servers, err := s.p.servers()
	if err != nil {
		klog.V(4).Infof("fuse sweeper: list fuse servers: %v", err)
		return
	}

	seen := make(map[int]bool, len(conns))
	for _, minor := range conns {
		mps := mounts[minor]
		if len(mps) == 0 {
			continue // foreign connection (not one of ours) — never touch
		}
		seen[minor] = true

		w, err := s.p.waiting(minor)
		if err != nil {
			// Connection vanished mid-sweep or is unreadable — reset and skip.
			delete(s.orphanStreak, minor)
			delete(s.aborted, minor)
			continue
		}

		// Orphaned = the kernel is waiting on requests AND no live process is
		// serving the mount. The liveness check is positive proof of life, so
		// we abort only when it is definitively false.
		if w <= 0 || mountHasLiveDaemon(mps, servers) {
			delete(s.orphanStreak, minor)
			delete(s.aborted, minor)
			continue
		}

		s.orphanStreak[minor]++
		if s.orphanStreak[minor] < orphanConfirmSweeps {
			klog.V(2).Infof("fuse sweeper: minor=%d looks orphaned (waiting=%d) at %v; confirming next sweep", minor, w, mps)
			continue
		}
		if s.aborted[minor] {
			continue // already aborted; waiting for the kernel to tear it down
		}
		klog.Warningf("fuse sweeper: aborting orphaned FUSE connection minor=%d (waiting=%d, no live daemon) mounts=%v", minor, w, mps)
		if err := s.p.abort(minor); err != nil {
			klog.Warningf("fuse sweeper: abort minor=%d: %v", minor, err)
			continue
		}
		s.aborted[minor] = true
	}

	// Prune state for minors no longer present so the maps cannot grow unbounded.
	for minor := range s.orphanStreak {
		if !seen[minor] {
			delete(s.orphanStreak, minor)
		}
	}
	for minor := range s.aborted {
		if !seen[minor] {
			delete(s.aborted, minor)
		}
	}
}

// mountHasLiveDaemon reports whether any live process is serving one of the
// given mountpoints (all referencing the same connection minor). This is the
// safety gate: the sweep aborts only when this is false, so a connection whose
// daemon is still running is never torn down.
//
// Two association signals cover both mount modes:
//   - mount-pod mode: the hf-mount daemon's argv carries the in-container mount
//     path /mnt/hf/<volumeID>; the host source path shares the <volumeID>
//     basename, and a bind target resolves to the same volumeID via mountID.
//   - sidecar mode: the daemon runs inside the workload pod and reads its args
//     from a file (no path in argv), so we match the pod UID against its cgroup.
func mountHasLiveDaemon(mountpoints []string, servers []fuseServerProc) bool {
	var volumeIDs, podUIDs []string
	for _, mp := range mountpoints {
		if v, ok := volumeIDForMount(mp); ok {
			volumeIDs = append(volumeIDs, v)
		}
		if uid, ok := podUIDForMount(mp); ok {
			podUIDs = append(podUIDs, uid)
		}
	}
	for _, srv := range servers {
		for _, v := range volumeIDs {
			if v != "" && strings.Contains(srv.cmdline, v) {
				return true
			}
		}
		for _, uid := range podUIDs {
			if uid != "" && cgroupHasPodUID(srv.cgroup, uid) {
				return true
			}
		}
	}
	return false
}

// isOurFuseMount reports whether a fuse mount belongs to this driver. Scoping is
// strict: either the mountpoint is under our source dir, or the mount source is
// our sentinel — a foreign CSI driver matches neither.
func isOurFuseMount(fm fuseMount) bool {
	if !strings.HasPrefix(fm.fstype, "fuse") {
		return false
	}
	if strings.HasPrefix(fm.mountpoint, mountBaseDir+"/") {
		return true
	}
	return fm.source == fuseMountSource
}

// volumeIDForMount returns the volume ID for one of our mountpoints: the
// basename for a source mount under mountBaseDir, or mountID(target) for a
// kubelet CSI publish target (which equals the volumeID the mount pod was
// named after).
func volumeIDForMount(mountpoint string) (string, bool) {
	if strings.HasPrefix(mountpoint, mountBaseDir+"/") {
		return filepath.Base(mountpoint), true
	}
	if strings.Contains(mountpoint, kubeletCSIMarker) {
		return mountID(mountpoint), true
	}
	return "", false
}

// podUIDForMount extracts the workload pod UID from a kubelet CSI publish target
// path of the form .../pods/<uid>/volumes/kubernetes.io~csi/<vol>/mount.
func podUIDForMount(mountpoint string) (string, bool) {
	if !strings.Contains(mountpoint, kubeletCSIMarker) {
		return "", false
	}
	const marker = "/pods/"
	i := strings.Index(mountpoint, marker)
	if i < 0 {
		return "", false
	}
	rest := mountpoint[i+len(marker):]
	j := strings.IndexByte(rest, '/')
	if j <= 0 {
		return "", false
	}
	return rest[:j], true
}

// cgroupHasPodUID reports whether a /proc/<pid>/cgroup blob references podUID.
// kubelet writes the UID into the cgroup path but the cgroup driver mangles the
// separators (systemd turns dashes into underscores and adds a pod prefix), so
// we strip dashes/underscores from both sides before matching.
func cgroupHasPodUID(cgroup, podUID string) bool {
	norm := func(s string) string {
		s = strings.ToLower(s)
		s = strings.ReplaceAll(s, "_", "")
		s = strings.ReplaceAll(s, "-", "")
		return s
	}
	nu := norm(podUID)
	if nu == "" {
		return false
	}
	return strings.Contains(norm(cgroup), nu)
}
