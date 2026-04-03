//go:build !linux

package driver

import "fmt"

func sidecarMount(sourceType, sourceID, target string, opts MountOptions, volumeName string) error {
	return fmt.Errorf("sidecar mount not supported on this platform")
}

func checkSidecarHealth(_, _ string) error { return nil }
func cleanupSidecarSocket(_ string)        {}
func refreshSidecarToken(_, _, _ string)   {}
