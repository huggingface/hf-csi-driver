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
	ExtraArgs      []string // passthrough flags from PV mountOptions
	TokenFile      string   // path to a file where the token is written for live refresh
	WorkloadPodUID string   // UID of the workload pod consuming this volume
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

func buildArgs(sourceType, sourceID, target string, opts MountOptions) ([]string, error) {
	switch sourceType {
	case "bucket", "repo":
	default:
		return nil, fmt.Errorf("unsupported sourceType: %q (must be \"bucket\" or \"repo\")", sourceType)
	}

	var globalArgs []string
	if opts.HubEndpoint != "" {
		globalArgs = append(globalArgs, "--hub-endpoint", opts.HubEndpoint)
	}
	if opts.CacheDir != "" {
		globalArgs = append(globalArgs, "--cache-dir", opts.CacheDir)
	}
	if opts.CacheSize != "" {
		globalArgs = append(globalArgs, "--cache-size", opts.CacheSize)
	}
	if opts.PollIntervalSecs != "" {
		globalArgs = append(globalArgs, "--poll-interval-secs", opts.PollIntervalSecs)
	}
	if opts.MetadataTtlMs != "" {
		globalArgs = append(globalArgs, "--metadata-ttl-ms", opts.MetadataTtlMs)
	}
	if opts.ReadOnly {
		globalArgs = append(globalArgs, "--read-only")
	}
	if opts.TokenFile != "" {
		globalArgs = append(globalArgs, "--token-file", opts.TokenFile)
	}

	globalArgs = append(globalArgs, opts.ExtraArgs...)
	args := append(globalArgs, sourceType, sourceID, target)

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
