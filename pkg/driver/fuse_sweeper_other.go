//go:build !linux

package driver

import "time"

// StartFuseSweeper is a no-op on non-Linux platforms (no /sys/fs/fuse, /proc).
func StartFuseSweeper(time.Duration, <-chan struct{}) {}
