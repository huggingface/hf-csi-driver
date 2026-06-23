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
		connections:    listFuseConnections,
		waiting:        fuseConnWaiting,
		ourMounts:      listOurFuseMounts,
		servers:        listFuseServers,
		abort:          abortFuseMinor,
		handoffPending: sidecarHandoffPending,
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

	mounts, err := parseFuseMounts(f)
	if err != nil {
		return nil, err
	}
	out := make(map[int][]string)
	for _, fm := range mounts {
		if isOurFuseMount(fm) {
			out[fm.minor] = append(out[fm.minor], fm.mountpoint)
		}
	}
	return out, nil
}

// listFuseServers enumerates every process holding an open /dev/fuse fd.
//
// Fail-safe is tri-state, because a false "no live daemon" is what would let us
// abort a live connection: a process that simply *vanished* mid-scan (ENOENT) is
// gone and skipped, but any *other* read error (permission, transient I/O) is
// treated as uncertainty and aborts the whole sweep — we would rather skip a
// sweep than misread an unreadable live daemon as dead.
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
		holds, err := processHoldsDevFuse(pid)
		if err != nil {
			if isGone(err) {
				continue
			}
			return nil, err // uncertain — skip the sweep
		}
		if !holds {
			continue
		}
		cmdline, err := readProcField(filepath.Join(procDir, e.Name(), "cmdline"))
		if err != nil {
			if isGone(err) {
				continue
			}
			return nil, err
		}
		cgroup, err := readProcField(filepath.Join(procDir, e.Name(), "cgroup"))
		if err != nil {
			if isGone(err) {
				continue
			}
			return nil, err
		}
		servers = append(servers, fuseServerProc{
			pid:     pid,
			cmdline: cmdlineToString(cmdline),
			cgroup:  string(cgroup),
		})
	}
	return servers, nil
}

// processHoldsDevFuse reports whether pid has any fd pointing at /dev/fuse. A
// zombie has already closed its fds, so it reads as not holding — exactly how we
// distinguish a dead daemon from a live one. Returns a non-ENOENT error as
// uncertainty (see listFuseServers).
func processHoldsDevFuse(pid int) (bool, error) {
	fdDir := filepath.Join(procDir, strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		if isGone(err) {
			return false, nil
		}
		return false, err
	}
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(fdDir, e.Name()))
		if err != nil {
			if isGone(err) {
				continue // fd closed mid-scan
			}
			return false, err
		}
		if target == devFuse {
			return true, nil
		}
	}
	return false, nil
}

// isGone reports whether err means the proc entry vanished (process or fd
// exited) — safe to skip. Any other error is uncertainty the caller must not
// swallow into "dead daemon".
func isGone(err error) bool {
	return os.IsNotExist(err)
}

func readProcField(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// cmdlineToString renders NUL-separated, NUL-terminated argv as a space-joined
// string.
func cmdlineToString(b []byte) string {
	return strings.ReplaceAll(strings.Trim(string(b), "\x00"), "\x00", " ")
}

func abortFuseMinor(minor int) error {
	return os.WriteFile(filepath.Join(fuseConnDir, strconv.Itoa(minor), "abort"), []byte("1"), 0o200)
}
