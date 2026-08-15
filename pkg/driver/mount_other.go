//go:build !linux

package driver

import "fmt"

// bindMount is not supported on non-Linux platforms.
func bindMount(source, target string) error {
	return fmt.Errorf("bind mount not supported on this platform")
}

// boundToCurrentSource is not supported on non-Linux platforms; always
// report false so callers fall through to the (unsupported) bind path.
func boundToCurrentSource(target, source string) bool {
	return false
}
