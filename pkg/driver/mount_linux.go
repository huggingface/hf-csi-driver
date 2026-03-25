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

// findBindMounts returns all mount points that share the same filesystem
// instance (major:minor device) as source. This finds bind mounts of a FUSE
// mount by parsing /proc/self/mountinfo.
func findBindMounts(source string) []string {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	defer f.Close()

	type mountEntry struct {
		devID     string
		mountPath string
	}

	var sourceDevID string
	var entries []mountEntry

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		devID := fields[2]     // major:minor
		mountPath := fields[4] // mount point
		entries = append(entries, mountEntry{devID: devID, mountPath: mountPath})
		if mountPath == source {
			sourceDevID = devID
		}
	}

	if sourceDevID == "" {
		return nil
	}

	var refs []string
	for _, entry := range entries {
		if entry.devID == sourceDevID && entry.mountPath != source {
			refs = append(refs, entry.mountPath)
		}
	}
	return refs
}

// mountRefCount returns how many other mount points reference the same
// filesystem as source (i.e. how many bind mounts exist for it).
func mountRefCount(source string) int {
	return len(findBindMounts(source))
}

// isMountStale checks if a mount at target is stale (FUSE transport dead).
func isMountStale(target string) bool {
	_, err := os.Stat(target)
	if err == nil {
		return false
	}
	if pe, ok := err.(*os.PathError); ok {
		if pe.Err == syscall.ENOTCONN || pe.Err == syscall.EIO {
			return true
		}
	}
	return false
}
