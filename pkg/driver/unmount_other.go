//go:build !linux

package driver

import "syscall"

// fuseUnmount performs a regular unmount on non-Linux platforms.
func fuseUnmount(target string) error {
	return syscall.Unmount(target, 0)
}

// abortFuseConnection is a no-op on non-Linux platforms (no /sys/fs/fuse).
func abortFuseConnection(string) {}
