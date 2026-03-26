//go:build linux

package driver

import (
	"os"
	"syscall"
)

// bindMount performs a bind mount from source to target.
func bindMount(source, target string) error {
	return syscall.Mount(source, target, "", syscall.MS_BIND, "")
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
