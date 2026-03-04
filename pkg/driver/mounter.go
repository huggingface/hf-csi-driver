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
	hfMountBinary   = "hf-mount-fuse"
	mountReadyPoll  = 100 * time.Millisecond
	mountTimeout    = 30 * time.Second
	stopGracePeriod = 10 * time.Second
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
	HFToken          string
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

// refMutex is a reference-counted mutex that can be safely cleaned up
// when no goroutine holds a reference to it.
type refMutex struct {
	sync.Mutex
	refs int
}

type ProcessMounter struct {
	mu      sync.Mutex
	mounts  map[string]*mountInfo
	locks   map[string]*refMutex
	checker mount.Interface
}

func NewProcessMounter() *ProcessMounter {
	return &ProcessMounter{
		mounts:  make(map[string]*mountInfo),
		locks:   make(map[string]*refMutex),
		checker: mount.New(""),
	}
}

// acquireTargetLock returns a per-target mutex with its refcount incremented.
// The caller must call releaseTargetLock when done.
func (m *ProcessMounter) acquireTargetLock(target string) *refMutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	lk, ok := m.locks[target]
	if !ok {
		lk = &refMutex{}
		m.locks[target] = lk
	}
	lk.refs++
	return lk
}

// releaseTargetLock decrements the refcount and removes the lock entry if
// no other goroutine holds a reference.
func (m *ProcessMounter) releaseTargetLock(target string, lk *refMutex) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lk.refs--
	if lk.refs == 0 {
		delete(m.locks, target)
	}
}

func (m *ProcessMounter) Mount(sourceType, sourceID, target string, opts MountOptions) error {
	tl := m.acquireTargetLock(target)
	tl.Lock()
	defer func() {
		tl.Unlock()
		m.releaseTargetLock(target, tl)
	}()

	args, err := buildArgs(sourceType, sourceID, target, opts)
	if err != nil {
		return err
	}

	cmd := exec.Command(hfMountBinary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = buildEnv(opts.HFToken)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	klog.Infof("Starting %s %s", hfMountBinary, strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start %s: %w", hfMountBinary, err)
	}

	done := make(chan struct{})
	info := &mountInfo{cmd: cmd, done: done}

	// Start crash detection goroutine immediately so done channel is always serviced.
	// Acquire its own target lock reference to prevent cleanup race.
	crashTl := m.acquireTargetLock(target)
	go func() {
		defer m.releaseTargetLock(target, crashTl)

		waitErr := cmd.Wait()
		// Close done FIRST so killProcess (which waits on done under tl) can
		// complete and release tl before we try to acquire it.
		close(done)

		// Acquire the per-target lock to serialize with Mount/Unmount and
		// atomically check ownership + cleanup without TOCTOU races.
		crashTl.Lock()
		defer crashTl.Unlock()

		m.mu.Lock()
		current, exists := m.mounts[target]
		isOwner := exists && current == info
		if isOwner {
			delete(m.mounts, target)
		}
		m.mu.Unlock()

		if isOwner {
			// Unexpected crash: log as warning and clean up FUSE mount.
			klog.Warningf("%s for %s crashed: %v", hfMountBinary, target, waitErr)
			if umountErr := fuseUnmount(target); umountErr != nil {
				klog.Warningf("unmount cleanup for %s failed: %v", target, umountErr)
			}
		} else {
			// Expected exit (Unmount already cleaned up).
			klog.V(4).Infof("%s for %s exited: %v", hfMountBinary, target, waitErr)
		}
	}()

	m.mu.Lock()
	m.mounts[target] = info
	m.mu.Unlock()

	// Wait for mount point to become ready (per-target lock still held,
	// preventing concurrent mount attempts on the same target).
	mountErr := m.waitForMount(target, done)

	if mountErr != nil {
		// killProcess does not need m.mu, only waits on done channel.
		_ = m.killProcess(info)
		m.mu.Lock()
		if m.mounts[target] == info {
			delete(m.mounts, target)
		}
		m.mu.Unlock()
		return fmt.Errorf("mount point %s did not become ready: %w", target, mountErr)
	}

	klog.Infof("Successfully mounted %s %s at %s", sourceType, sourceID, target)
	return nil
}

func (m *ProcessMounter) waitForMount(target string, processDone <-chan struct{}) error {
	deadline := time.After(mountTimeout)
	ticker := time.NewTicker(mountReadyPoll)
	defer ticker.Stop()

	var lastErr error
	for {
		select {
		case <-deadline:
			if lastErr != nil {
				return fmt.Errorf("timeout waiting for mount: %w", lastErr)
			}
			return fmt.Errorf("timeout waiting for mount")
		case <-processDone:
			return fmt.Errorf("mount process exited before mount became ready")
		case <-ticker.C:
			mounted, err := m.checker.IsMountPoint(target)
			if err != nil {
				lastErr = err
				continue
			}
			if mounted {
				return nil
			}
		}
	}
}

func (m *ProcessMounter) Unmount(target string) error {
	tl := m.acquireTargetLock(target)
	tl.Lock()
	defer func() {
		tl.Unlock()
		m.releaseTargetLock(target, tl)
	}()

	m.mu.Lock()
	info, tracked := m.mounts[target]
	if tracked {
		delete(m.mounts, target)
	}
	m.mu.Unlock()

	if tracked {
		if err := m.killProcess(info); err != nil {
			klog.Warningf("Failed to stop process for %s: %v", target, err)
		}
	}

	// Lazy unmount to clean up stale FUSE mounts (e.g. from a previous DaemonSet).
	if err := fuseUnmount(target); err != nil {
		klog.V(4).Infof("unmount %s: %v", target, err)
	}

	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove mount target %s: %w", target, err)
	}
	return nil
}

func (m *ProcessMounter) IsMountPoint(target string) (bool, error) {
	return m.checker.IsMountPoint(target)
}

// killProcess sends SIGTERM then SIGKILL to the process group.
// Must NOT be called while holding m.mu (it waits on info.done which the
// crash goroutine closes after acquiring m.mu).
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

// buildEnv returns the environment for the hf-mount-fuse process.
// If token is non-empty it overrides any inherited HF_TOKEN.
func buildEnv(token string) []string {
	env := os.Environ()
	if token == "" {
		return env
	}
	// Replace or append HF_TOKEN.
	set := false
	for i, e := range env {
		if strings.HasPrefix(e, "HF_TOKEN=") {
			env[i] = "HF_TOKEN=" + token
			set = true
			break
		}
	}
	if !set {
		env = append(env, "HF_TOKEN="+token)
	}
	return env
}

func buildArgs(sourceType, sourceID, target string, opts MountOptions) ([]string, error) {
	switch sourceType {
	case "bucket", "repo":
	default:
		return nil, fmt.Errorf("unsupported sourceType: %q (must be \"bucket\" or \"repo\")", sourceType)
	}

	// Global options (before subcommand).
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
	if opts.AdvancedWrites {
		globalArgs = append(globalArgs, "--advanced-writes")
	}
	if opts.UID != "" {
		globalArgs = append(globalArgs, "--uid", opts.UID)
	}
	if opts.GID != "" {
		globalArgs = append(globalArgs, "--gid", opts.GID)
	}

	// Subcommand + positional args.
	args := append(globalArgs, sourceType, sourceID, target)

	// Subcommand-specific flags.
	if sourceType == "repo" && opts.Revision != "" {
		args = append(args, "--revision", opts.Revision)
	}
	args = append(args, opts.ExtraArgs...)

	return args, nil
}
