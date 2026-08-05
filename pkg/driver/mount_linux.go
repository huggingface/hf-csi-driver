//go:build linux

package driver

import (
	"bufio"
	"os"
	"strings"
	"syscall"
)

// bindMount performs a bind mount from source to target.
func bindMount(source, target string) error {
	return syscall.Mount(source, target, "", syscall.MS_BIND, "")
}

// boundToCurrentSource reports whether target's topmost mount is backed by
// the same superblock as source's topmost mount, per /proc/self/mountinfo.
// mountinfo is never served from the kernel attribute cache, so — unlike a
// stat() probe — it cannot mistake a dead FUSE bind for a live one right
// after the daemon dies.
func boundToCurrentSource(target, source string) bool {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	// mountinfo fields: id parent major:minor root mountpoint ...
	// The LAST entry for a mountpoint is the topmost mount there. Identify a
	// mount's backing by (major:minor, root).
	var targetTop, sourceTop string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		backing := fields[2] + " " + fields[3]
		switch fields[4] {
		case target:
			targetTop = backing
		case source:
			sourceTop = backing
		}
	}
	return targetTop != "" && targetTop == sourceTop
}
