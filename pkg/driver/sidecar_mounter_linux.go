//go:build linux

package driver

import (
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/huggingface/hf-buckets-csi-driver/pkg/util"
	"k8s.io/klog/v2"
)

const (
	// emptyDirName is the name of the shared emptyDir volume injected by
	// the webhook. Used to locate the per-pod communication directory on
	// the host filesystem.
	emptyDirName = "hf-csi-tmp"

	// kubeletPodsBase is where kubelet stores per-pod volume data on the host.
	kubeletPodsBase = "/var/lib/kubelet/pods"

	// socketAcceptTimeout is how long the CSI driver waits for the sidecar
	// to connect and receive the FUSE fd. If the sidecar doesn't connect
	// (e.g. image pull failure), the fd is closed and the mount becomes stale.
	socketAcceptTimeout = 5 * time.Minute

	// symlinkBase is a host directory for short symlinks to the emptyDir
	// volume directories. Unix socket paths are limited to 108 characters,
	// so we use /var/run/hf-csi/<hash> -> <long kubelet path>.
	symlinkBase = "/var/run/hf-csi"

	// sidecarFuseFdCount is how many /dev/fuse fds (1 primary + N-1 clones)
	// the CSI driver pre-creates per volume and ships to the sidecar. The
	// sidecar runs one reader thread per fd; the bottleneck is typically
	// network so single-digit counts are plenty.
	sidecarFuseFdCount = 4

	// fuseDevIoctlClone is the FUSE_DEV_IOC_CLONE ioctl number, encoded by
	// _IOR(229, 0, uint32_t). Cloning binds the new fd to the same FUSE
	// connection without reopening /dev/fuse.
	fuseDevIoctlClone = 0x8004E500
)

// cloneFuseFd issues FUSE_DEV_IOC_CLONE to bind a fresh fd to the same
// FUSE connection as `src`. The new fd is returned; the caller owns it
// and must close it.
func cloneFuseFd(src int) (int, error) {
	dst, err := syscall.Open("/dev/fuse", syscall.O_RDWR, 0o644)
	if err != nil {
		return -1, fmt.Errorf("open /dev/fuse for clone: %w", err)
	}
	srcFd := uint32(src)
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(dst), uintptr(fuseDevIoctlClone), uintptr(unsafe.Pointer(&srcFd)))
	if errno != 0 {
		_ = syscall.Close(dst)
		return -1, fmt.Errorf("FUSE_DEV_IOC_CLONE: %w", errno)
	}
	return dst, nil
}

// sidecarMountPath is where the sidecar sees the shared emptyDir.
const sidecarMountPath = "/hf-csi-tmp"

// sidecarPath converts a host-side emptyDir path to the sidecar-visible path.
// Host: /var/lib/kubelet/pods/<uid>/volumes/kubernetes.io~empty-dir/hf-csi-tmp/.volumes/<hash>/token
// Sidecar: /hf-csi-tmp/.volumes/<hash>/token
func sidecarPath(hostPath string) string {
	const marker = emptyDirName + "/"
	idx := strings.Index(hostPath, marker)
	if idx < 0 {
		return hostPath
	}
	return filepath.Join(sidecarMountPath, hostPath[idx+len(marker):])
}

// sidecarEmptyDirPath returns the host path of the shared emptyDir for the
// given pod. This is where the CSI driver writes args files and creates
// sockets, and where the sidecar reads them.
func sidecarEmptyDirPath(podUID string) string {
	return filepath.Join(kubeletPodsBase, podUID, "volumes", "kubernetes.io~empty-dir", emptyDirName)
}

// sidecarMount performs a FUSE mount via fd-passing to the sidecar container.
//
// Flow:
//  1. Open /dev/fuse and perform the kernel FUSE mount (syscall.Mount)
//  2. Write an args file to the shared emptyDir (same CLI syntax as hf-mount-fuse)
//  3. Create a Unix socket and wait for the sidecar to connect
//  4. Send the /dev/fuse fd to the sidecar via SCM_RIGHTS
//
// After step 1, the mount point is valid (stat succeeds) even without a
// FUSE daemon. Reads block in the kernel until the sidecar starts serving.
func sidecarMount(sourceType, sourceID, target string, opts MountOptions, volumeName string) error {
	podUID := opts.WorkloadPodUID
	tmpDir := sidecarEmptyDirPath(podUID)

	// --- Step 1: Open /dev/fuse and do the kernel mount ---

	fd, err := syscall.Open("/dev/fuse", syscall.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open /dev/fuse: %w", err)
	}
	klog.V(4).Infof("Opened /dev/fuse: fd=%d", fd)

	// We use syscall.Mount() directly instead of /bin/mount because the
	// mount.fuse helper is not available in our minimal container image.
	// When the pod specifies an fsGroup (via VOLUME_MOUNT_GROUP), use it
	// for the FUSE mount user_id and group_id so the mount point itself
	// is owned by the pod's user.
	fuseUID, fuseGID := 0, 0
	if opts.VolumeMountGroup != "" {
		if v, err := strconv.Atoi(opts.VolumeMountGroup); err == nil && v > 0 {
			fuseUID = v
			fuseGID = v
		}
	}
	flags := uintptr(syscall.MS_NODEV | syscall.MS_NOSUID)
	mountData := fmt.Sprintf("fd=%d,rootmode=40755,user_id=%d,group_id=%d,allow_other,default_permissions", fd, fuseUID, fuseGID)
	if err := syscall.Mount("hf-mount", target, "fuse", flags, mountData); err != nil {
		_ = syscall.Close(fd)
		return fmt.Errorf("fuse mount on %s: %w", target, err)
	}
	klog.Infof("Kernel FUSE mount at %s (fd=%d)", target, fd)

	// Clean up both the mount and the fd if anything below fails.
	cleanup := func() {
		_ = syscall.Unmount(target, 0)
		_ = syscall.Close(fd)
	}

	// --- Step 2: Write the args file for the sidecar ---

	// Each volume gets its own directory: <emptyDir>/.volumes/<hash>/
	// The hash is derived from the volume name to avoid path collisions.
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(volumeName)))[:12]
	volumeDir := filepath.Join(tmpDir, ".volumes", hash)
	if err := os.MkdirAll(volumeDir, 0755); err != nil {
		cleanup()
		return fmt.Errorf("create volume dir: %w", err)
	}
	// The sidecar (user 65534) needs to write ready/error files here.
	_ = os.Chown(volumeDir, 65534, 65534)

	// Copy the token to the emptyDir so the sidecar can read it via --token-file.
	// The emptyDir is tmpfs (medium: Memory) so the token never hits disk.
	// On republish, refreshSidecarToken overwrites this file with fresh credentials.
	if opts.TokenFile != "" {
		tokenDst := filepath.Join(volumeDir, "token")
		if data, err := os.ReadFile(opts.TokenFile); err == nil {
			if err := os.WriteFile(tokenDst, data, 0600); err != nil {
				cleanup()
				return fmt.Errorf("write sidecar token: %w", err)
			}
			// The sidecar runs as user 65534 (nobody) and needs to read the token.
			_ = os.Chown(tokenDst, 65534, 65534)
		}
		opts.TokenFile = sidecarPath(tokenDst)
	}

	// Clear CacheDir: the host path is inaccessible from the sidecar.
	// hf-mount will use its default (/tmp/hf-mount-cache).
	opts.CacheDir = ""

	args, err := buildArgs(sourceType, sourceID, "/tmp", opts)
	if err != nil {
		cleanup()
		return fmt.Errorf("build args: %w", err)
	}
	// Prepend program name (required by clap's try_parse_from).
	argsContent := "hf-mount-fuse-sidecar\n" + strings.Join(args, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(volumeDir, "args"), []byte(argsContent), 0644); err != nil {
		cleanup()
		return fmt.Errorf("write args: %w", err)
	}

	// --- Step 3: Create a Unix socket for fd-passing ---

	// Unix socket paths are limited to 108 chars. The kubelet emptyDir path
	// is too long, so we create a short symlink: /var/run/hf-csi/<hash> -> volumeDir.
	// net.Listen on the symlink path creates the socket file in volumeDir
	// (kernel resolves symlink directory components during bind).
	if err := os.MkdirAll(symlinkBase, 0755); err != nil {
		cleanup()
		return fmt.Errorf("create symlink base: %w", err)
	}
	symlinkPath := filepath.Join(symlinkBase, hash)
	_ = os.Remove(symlinkPath) // clean up stale symlink from a previous mount
	if err := os.Symlink(volumeDir, symlinkPath); err != nil {
		cleanup()
		return fmt.Errorf("create symlink: %w", err)
	}

	socketPath := filepath.Join(symlinkPath, "s")
	_ = os.Remove(filepath.Join(volumeDir, "s")) // clean up stale socket
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		cleanup()
		return fmt.Errorf("listen on %s: %w", socketPath, err)
	}

	// The sidecar runs as user 65534 (nobody). It needs write permission
	// on the socket file to connect.
	_ = os.Chown(filepath.Join(volumeDir, "s"), 65534, 65534)

	// --- Step 4: Async goroutine sends the fd when the sidecar connects ---

	// NodePublishVolume returns immediately after the kernel mount (step 1).
	// The goroutine waits for the sidecar to connect and passes the fd.
	// If the sidecar never connects (timeout), the fd is closed and the
	// mount becomes stale (ENOTCONN on reads).
	go func() {
		defer func() {
			_ = listener.Close()
			klog.V(4).Infof("Closed socket listener %s", socketPath)
		}()
		defer func() {
			_ = os.Remove(symlinkPath)
			klog.V(4).Infof("Removed socket symlink %s", symlinkPath)
		}()

		_ = listener.(*net.UnixListener).SetDeadline(time.Now().Add(socketAcceptTimeout))

		klog.Infof("Waiting for sidecar to connect on %s (timeout %v)", socketPath, socketAcceptTimeout)
		conn, err := listener.Accept()
		if err != nil {
			klog.Errorf("Sidecar did not connect within %v: %v", socketAcceptTimeout, err)
			_ = syscall.Close(fd)
			return
		}
		defer func() { _ = conn.Close() }()

		// Pre-clone the FUSE fd so the unprivileged sidecar can run a
		// multi-threaded reader. The sidecar can't issue FUSE_DEV_IOC_CLONE
		// itself (the ioctl reopens /dev/fuse, which needs CAP_SYS_ADMIN).
		// We send the primary plus N-1 clones in a single SCM_RIGHTS cmsg.
		fds := []int{fd}
		for i := 1; i < sidecarFuseFdCount; i++ {
			cloned, err := cloneFuseFd(fd)
			if err != nil {
				klog.Warningf("clone /dev/fuse fd=%d (#%d): %v — falling back to fewer threads", fd, i, err)
				break
			}
			fds = append(fds, cloned)
		}

		// Send the fds via SCM_RIGHTS. The kernel duplicates each fd into
		// the sidecar's fd table. We pass nil as the data payload (the
		// sidecar reads its config from the args file, not from the socket).
		if err := util.SendMsgFds(conn, fds, nil); err != nil {
			klog.Errorf("SendMsgFds count=%d primary=%d: %v", len(fds), fd, err)
			for _, f := range fds {
				_ = syscall.Close(f)
			}
			return
		}

		klog.Infof("Sent %d fd(s) (primary=%d) to sidecar for %s %s", len(fds), fd, sourceType, sourceID)

		// The sidecar now owns duplicates of every fd. Close our copies
		// to avoid leaking N fds per volume on the CSI driver.
		for _, f := range fds {
			_ = syscall.Close(f)
		}
	}()

	return nil
}

// checkSidecarHealth reads the error file written by the sidecar when its
// FUSE daemon fails. Called during republish (NodePublishVolume on an
// already-mounted volume). If an error is found, the CSI driver returns a
// gRPC error, causing kubelet to emit a FailedMount event on the pod.
func checkSidecarHealth(podUID, volumeName string) error {
	tmpDir := sidecarEmptyDirPath(podUID)
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(volumeName)))[:12]
	errorPath := filepath.Join(tmpDir, ".volumes", hash, "error")

	data, err := os.ReadFile(errorPath)
	if err != nil {
		return nil // no error file = healthy
	}
	msg := strings.TrimSpace(string(data))
	if msg == "" {
		return nil
	}
	return fmt.Errorf("sidecar error: %s", msg)
}

// cleanupSidecarSocket removes the short symlink in /var/run/hf-csi/ created
// during sidecarMount. Called during NodeUnpublishVolume to prevent orphaned
// symlinks when the async goroutine times out after pod deletion.
func cleanupSidecarSocket(volumeName string) {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(volumeName)))[:12]
	symlinkPath := filepath.Join(symlinkBase, hash)
	_ = os.Remove(symlinkPath)
}

// refreshSidecarToken overwrites the token file in the sidecar's emptyDir.
// Called during republish when kubelet passes fresh secrets. hf-mount re-reads
// the --token-file before each Hub request, so no sidecar restart is needed.
func refreshSidecarToken(podUID, volumeName, token string) {
	tmpDir := sidecarEmptyDirPath(podUID)
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(volumeName)))[:12]
	tokenPath := filepath.Join(tmpDir, ".volumes", hash, "token")

	if err := os.WriteFile(tokenPath, []byte(token), 0600); err != nil {
		klog.Warningf("Sidecar token refresh: cannot write %s: %v", tokenPath, err)
	} else {
		_ = os.Chown(tokenPath, 65534, 65534)
		klog.V(4).Infof("Refreshed sidecar token for volume %s", volumeName)
	}
}
