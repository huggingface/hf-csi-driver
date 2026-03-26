//go:build !linux

package driver

import "fmt"

// bindMount is not supported on non-Linux platforms.
func bindMount(source, target string) error {
	return fmt.Errorf("bind mount not supported on this platform")
}

// isMountStale is not supported on non-Linux platforms.
func isMountStale(target string) bool {
	return false
}
