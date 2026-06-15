//go:build linux

package driver

import (
	"fmt"
	"os"
	"syscall"

	"k8s.io/klog/v2"
)

// fuseUnmount performs a lazy unmount (MNT_DETACH) of the given path.
//
// It does NOT abort the FUSE connection — callers that operate on a *direct*
// FUSE mount (the sidecar target) call abortFuseConnection separately first.
// We must not abort here because PodMounter also calls fuseUnmount on bind-mount
// references, which share the source mount's device (and thus its FUSE
// connection minor): aborting via a bind would tear down the still-live source
// connection for every other reference.
func fuseUnmount(target string) error {
	return syscall.Unmount(target, syscall.MNT_DETACH)
}

// abortFuseConnection resolves the FUSE device minor backing target and writes
// to /sys/fs/fuse/connections/<minor>/abort. It guarantees pod termination when
// the in-pod FUSE daemon (the hf-mount sidecar) has wedged: e.g. a thread stuck
// in an `inval_inode` writev blocked in the kernel on folio writeback. Such a
// thread is in uninterruptible D-state and survives `exit_group`, so the pod can
// never be reaped; MNT_DETACH alone never unblocks it. Aborting errors out every
// outstanding FUSE request, releasing the waiter so the sidecar exits.
//
// MUST only be called on a direct FUSE mount (the sidecar target), never on a
// bind-mount reference — a bind shares the source's connection minor.
//
// Best-effort: every failure is logged at low verbosity and ignored so the
// caller still attempts the unmount. We resolve the minor by parsing
// /proc/self/mountinfo rather than stat(target): stat() on a wedged FUSE mount
// would itself block in D-state, which is exactly what we are trying to avoid.
func abortFuseConnection(target string) {
	minor, ok := fuseMinorForMount(target)
	if !ok {
		return
	}
	abortPath := fmt.Sprintf("/sys/fs/fuse/connections/%d/abort", minor)
	if err := os.WriteFile(abortPath, []byte("1"), 0o200); err != nil {
		if !os.IsNotExist(err) {
			klog.V(4).Infof("fuse abort %s: %v", abortPath, err)
		}
		return
	}
	klog.Infof("Aborted FUSE connection minor=%d for %s", minor, target)
}

// fuseMinorForMount returns the device minor of the fuse mount at mountPoint by
// parsing /proc/self/mountinfo, or false if none matches.
func fuseMinorForMount(mountPoint string) (int, bool) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		klog.V(4).Infof("open mountinfo: %v", err)
		return 0, false
	}
	defer f.Close()
	return parseFuseMinor(f, mountPoint)
}
