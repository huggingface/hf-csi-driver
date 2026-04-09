package driver

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	hfMountBinary = "hf-mount-fuse"
)

type MountOptions struct {
	Revision         string
	HubEndpoint      string
	CacheDir         string
	CacheSize        string
	PollIntervalSecs string
	MetadataTtlMs    string
	ReadOnly         bool
	ExtraArgs        []string // passthrough flags from PV mountOptions
	TokenFile        string   // path to a file where the token is written for live refresh
	WorkloadPodUID   string   // UID of the workload pod consuming this volume
	VolumeMountGroup string   // fsGroup from pod security context, passed via CSI VOLUME_MOUNT_GROUP
}

type Mounter interface {
	Mount(sourceType, sourceID, target string, opts MountOptions) error
	Unmount(target string) error
	IsMountPoint(target string) (bool, error)
	// CheckHealth returns an error if the mount backing target is unhealthy
	// (e.g. mount pod in CrashLoopBackOff). Returns nil if healthy or unknown.
	CheckHealth(target string) error
	// Recover re-adopts existing mounts from a previous driver instance.
	Recover() error
	// Start launches background goroutines (e.g. pod watchers).
	Start(stopCh <-chan struct{})
}

// buildArgs builds the hf-mount-fuse CLI arguments from MountOptions.
// Used by both podmount (args passed to the mount pod command) and sidecar
// (args written to a file). The caller sets the right values in opts
// for each mode (e.g. sidecar clears CacheDir and rewrites TokenFile).
func buildArgs(sourceType, sourceID, target string, opts MountOptions) ([]string, error) {
	switch sourceType {
	case "bucket", "repo":
	default:
		return nil, fmt.Errorf("unsupported sourceType: %q (must be \"bucket\" or \"repo\")", sourceType)
	}

	var args []string
	if opts.HubEndpoint != "" {
		args = append(args, "--hub-endpoint", opts.HubEndpoint)
	}
	if opts.CacheDir != "" {
		args = append(args, "--cache-dir", opts.CacheDir)
	}
	if opts.CacheSize != "" {
		args = append(args, "--cache-size", opts.CacheSize)
	}
	if opts.PollIntervalSecs != "" {
		args = append(args, "--poll-interval-secs", opts.PollIntervalSecs)
	}
	if opts.MetadataTtlMs != "" {
		args = append(args, "--metadata-ttl-ms", opts.MetadataTtlMs)
	}
	if opts.ReadOnly {
		args = append(args, "--read-only")
	}
	if opts.TokenFile != "" {
		args = append(args, "--token-file", opts.TokenFile)
	}

	args = append(args, opts.ExtraArgs...)
	args = append(args, sourceType, sourceID, target)

	if sourceType == "repo" && opts.Revision != "" {
		args = append(args, "--revision", opts.Revision)
	}

	return args, nil
}

// writeTokenFile atomically writes a token to a file.
func writeTokenFile(path, token string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".token-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(token); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}
