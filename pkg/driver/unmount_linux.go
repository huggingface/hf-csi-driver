package driver

import "syscall"

// fuseUnmount performs a lazy unmount (MNT_DETACH) of the given path.
func fuseUnmount(target string) error {
	return syscall.Unmount(target, syscall.MNT_DETACH)
}
