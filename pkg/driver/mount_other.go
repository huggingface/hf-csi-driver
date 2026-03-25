//go:build !linux

package driver

import "fmt"

// bindMount is not supported on non-Linux platforms.
func bindMount(source, target string) error {
	return fmt.Errorf("bind mount not supported on this platform")
}

// findBindMounts is not supported on non-Linux platforms.
func findBindMounts(source string) []string {
	return nil
}

// mountRefCount is not supported on non-Linux platforms.
func mountRefCount(source string) int {
	return 0
}

// isMountStale is not supported on non-Linux platforms.
func isMountStale(target string) bool {
	return false
}
