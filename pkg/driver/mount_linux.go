//go:build linux

package driver

import (
	"bufio"
	"os"
	"strconv"
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
		switch unescapeMountPath(fields[4]) {
		case target:
			targetTop = backing
		case source:
			sourceTop = backing
		}
	}
	if scanner.Err() != nil {
		// A short read must not report "already bound": the caller then
		// stacks a redundant (harmless) bind rather than skipping a needed one.
		return false
	}
	return targetTop != "" && targetTop == sourceTop
}

// unescapeMountPath decodes the octal escapes mountinfo uses for special
// characters in path fields (`\040` space, `\011` tab, `\012` newline,
// `\134` backslash). Paths without a backslash are returned as-is.
func unescapeMountPath(p string) string {
	if !strings.Contains(p, "\\") {
		return p
	}
	var b strings.Builder
	b.Grow(len(p))
	for i := 0; i < len(p); i++ {
		if p[i] == '\\' && i+3 < len(p) {
			if v, err := strconv.ParseUint(p[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(p[i])
	}
	return b.String()
}
