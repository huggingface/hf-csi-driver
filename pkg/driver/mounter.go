package driver

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
)

const (
	hfMountBinary    = "hf-mount-fuse"
	fusermountBinary = "fusermount3"
	mountReadyPoll   = 100 * time.Millisecond
	mountTimeout     = 30 * time.Second
	stopGracePeriod  = 10 * time.Second
)

type MountOptions struct {
	Revision         string
	HubEndpoint      string
	CacheDir         string
	CacheSize        string
	PollIntervalSecs string
	MetadataTtlMs    string
	ReadOnly         bool
	AdvancedWrites   bool
	UID              string
	GID              string
	ExtraArgs        []string
}

type Mounter interface {
	Mount(sourceType, sourceID, target string, opts MountOptions) error
	Unmount(target string) error
	IsMountPoint(target string) (bool, error)
}

type mountInfo struct {
	cmd  *exec.Cmd
	done chan struct{}
}

type ProcessMounter struct {
	mu      sync.Mutex
	mounts  map[string]*mountInfo
	locks   map[string]*sync.Mutex
	checker mount.Interface
}

func NewProcessMounter() *ProcessMounter {
	return &ProcessMounter{
		mounts:  make(map[string]*mountInfo),
		locks:   make(map[string]*sync.Mutex),
		checker: mount.New(""),
	}
}

func (m *ProcessMounter) targetLock(target string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.locks[target]; !ok {
		m.locks[target] = &sync.Mutex{}
	}
	return m.locks[target]
}

func (m *ProcessMounter) Mount(sourceType, sourceID, target string, opts MountOptions) error {
	lock := m.targetLock(target)
	lock.Lock()
	defer lock.Unlock()

	args, err := buildArgs(sourceType, sourceID, target, opts)
	if err != nil {
		return err
	}

	cmd := exec.Command(hfMountBinary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	klog.Infof("Starting %s %s", hfMountBinary, strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start %s: %w", hfMountBinary, err)
	}

	done := make(chan struct{})
	info := &mountInfo{cmd: cmd, done: done}

	// Start crash detection goroutine immediately so done channel is always serviced.
	go func() {
		defer close(done)
		err := cmd.Wait()
		klog.Warningf("%s for %s exited: %v", hfMountBinary, target, err)
		// Lazy unmount to clean up stale FUSE mount.
		out, umountErr := exec.Command(fusermountBinary, "-u", "-z", target).CombinedOutput()
		if umountErr != nil {
			klog.Warningf("fusermount3 cleanup for %s failed: %v: %s", target, umountErr, string(out))
		}
		m.mu.Lock()
		delete(m.mounts, target)
		delete(m.locks, target)
		m.mu.Unlock()
	}()

	m.mu.Lock()
	m.mounts[target] = info
	m.mu.Unlock()

	// Wait for mount point to become ready.
	if err := m.waitForMount(target); err != nil {
		_ = m.killProcess(info)
		m.mu.Lock()
		delete(m.mounts, target)
		m.mu.Unlock()
		return fmt.Errorf("mount point %s did not become ready: %w", target, err)
	}

	klog.Infof("Successfully mounted %s %s at %s", sourceType, sourceID, target)
	return nil
}

func (m *ProcessMounter) waitForMount(target string) error {
	deadline := time.After(mountTimeout)
	ticker := time.NewTicker(mountReadyPoll)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return fmt.Errorf("timeout waiting for mount")
		case <-ticker.C:
			mounted, err := m.checker.IsMountPoint(target)
			if err == nil && mounted {
				return nil
			}
		}
	}
}

func (m *ProcessMounter) Unmount(target string) error {
	m.mu.Lock()
	info, tracked := m.mounts[target]
	m.mu.Unlock()

	if tracked {
		if err := m.killProcess(info); err != nil {
			klog.Warningf("Failed to stop process for %s: %v", target, err)
		}
		m.mu.Lock()
		delete(m.mounts, target)
		delete(m.locks, target)
		m.mu.Unlock()
	}

	// Always try fusermount3 as fallback (handles stale mounts from previous DaemonSet).
	out, err := exec.Command(fusermountBinary, "-u", "-z", target).CombinedOutput()
	if err != nil {
		klog.V(4).Infof("fusermount3 -u -z %s: %v: %s", target, err, string(out))
	}

	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove mount target %s: %w", target, err)
	}
	return nil
}

func (m *ProcessMounter) IsMountPoint(target string) (bool, error) {
	return m.checker.IsMountPoint(target)
}

func (m *ProcessMounter) killProcess(info *mountInfo) error {
	if info.cmd.Process == nil {
		return nil
	}
	pgid := -info.cmd.Process.Pid

	// SIGTERM the process group.
	if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil {
		klog.V(4).Infof("SIGTERM pgid %d: %v", pgid, err)
	}

	select {
	case <-info.done:
		return nil
	case <-time.After(stopGracePeriod):
	}

	// Force kill.
	klog.Warningf("Process %d did not exit after SIGTERM, sending SIGKILL", info.cmd.Process.Pid)
	if err := syscall.Kill(pgid, syscall.SIGKILL); err != nil {
		return fmt.Errorf("SIGKILL pgid %d: %w", pgid, err)
	}

	<-info.done
	return nil
}

func buildArgs(sourceType, sourceID, target string, opts MountOptions) ([]string, error) {
	switch sourceType {
	case "bucket", "repo":
	default:
		return nil, fmt.Errorf("unsupported sourceType: %q (must be \"bucket\" or \"repo\")", sourceType)
	}

	args := []string{sourceType, sourceID, target}

	if sourceType == "repo" && opts.Revision != "" {
		args = append(args, "--revision", opts.Revision)
	}
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
	if opts.AdvancedWrites {
		args = append(args, "--advanced-writes")
	}
	if opts.UID != "" {
		args = append(args, "--uid", opts.UID)
	}
	if opts.GID != "" {
		args = append(args, "--gid", opts.GID)
	}
	args = append(args, opts.ExtraArgs...)

	return args, nil
}
