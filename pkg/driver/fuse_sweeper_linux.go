//go:build linux

package driver

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	fuseConnDir = "/sys/fs/fuse/connections"
	// hostMountinfo is PID 1's mount table. With hostPID:true on the node
	// DaemonSet, PID 1 is the host init, so this is the authoritative host mount
	// namespace — the same one kubelet and the mount/sidecar pods mount into.
	hostMountinfo = "/proc/1/mountinfo"
	procDir       = "/proc"
	devFuse       = "/dev/fuse"
)

// StartFuseSweeper runs the orphaned-FUSE-connection sweep until stopCh closes.
func StartFuseSweeper(interval time.Duration, stopCh <-chan struct{}) {
	s := newFuseSweeper(fuseSweepProviders{
		connections: listFuseConnections,
		waiting:     fuseConnWaiting,
		ourMounts:   listOurFuseMounts,
		servers:     listFuseServers,
		abort:       abortFuseMinor,
	}, interval)
	s.run(stopCh)
}

// listFuseConnections returns the connection minors under
// /sys/fs/fuse/connections. A missing dir (fusectl not mounted) is not an error.
func listFuseConnections() ([]int, error) {
	entries, err := os.ReadDir(fuseConnDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var minors []int
	for _, e := range entries {
		if m, err := strconv.Atoi(e.Name()); err == nil {
			minors = append(minors, m)
		}
	}
	return minors, nil
}

func fuseConnWaiting(minor int) (int64, error) {
	b, err := os.ReadFile(filepath.Join(fuseConnDir, strconv.Itoa(minor), "waiting"))
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

// listOurFuseMounts parses the host mount table and returns minor -> mountpoints
// for fuse mounts that belong to this driver (see isOurFuseMount).
func listOurFuseMounts() (map[int][]string, error) {
	f, err := os.Open(hostMountinfo)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	out := make(map[int][]string)
	for _, fm := range parseFuseMounts(f) {
		if isOurFuseMount(fm) {
			out[fm.minor] = append(out[fm.minor], fm.mountpoint)
		}
	}
	return out, nil
}

// listFuseServers enumerates every process holding an open /dev/fuse fd. A
// top-level /proc read failure is returned as an error (so the sweep skips
// rather than mistaking it for "no daemons"); per-process errors (a process
// exiting mid-scan, or unreadable fds) are skipped silently.
func listFuseServers() ([]fuseServerProc, error) {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return nil, err
	}
	var servers []fuseServerProc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a pid directory
		}
		if !processHoldsDevFuse(pid) {
			continue
		}
		servers = append(servers, fuseServerProc{
			pid:     pid,
			cmdline: readProcCmdline(pid),
			cgroup:  readProcFile(filepath.Join(procDir, e.Name(), "cgroup")),
		})
	}
	return servers, nil
}

// processHoldsDevFuse reports whether pid has any fd pointing at /dev/fuse. A
// zombie has already closed its fds, so it reads as not holding — exactly how we
// distinguish a dead daemon from a live one.
func processHoldsDevFuse(pid int) bool {
	fdDir := filepath.Join(procDir, strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(fdDir, e.Name()))
		if err != nil {
			continue
		}
		if target == devFuse {
			return true
		}
	}
	return false
}

func readProcCmdline(pid int) string {
	b, err := os.ReadFile(filepath.Join(procDir, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return ""
	}
	// argv is NUL-separated and NUL-terminated; render it space-joined.
	return strings.ReplaceAll(strings.Trim(string(b), "\x00"), "\x00", " ")
}

func readProcFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func abortFuseMinor(minor int) error {
	return os.WriteFile(filepath.Join(fuseConnDir, strconv.Itoa(minor), "abort"), []byte("1"), 0o200)
}
