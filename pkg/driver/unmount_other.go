//go:build !linux

package driver

import "syscall"

// fuseUnmount performs a regular unmount on non-Linux platforms.
func fuseUnmount(target string) error {
	return syscall.Unmount(target, 0)
}
