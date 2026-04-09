package driver

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
)

const (
	defaultRevision = "main"
	defaultTokenKey = "token"

	volumeCtxSourceType   = "sourceType"
	volumeCtxSourceID     = "sourceId"
	volumeCtxRevision     = "revision"
	volumeCtxHubEndpoint  = "hubEndpoint"
	volumeCtxCacheDir     = "cacheDir"
	volumeCtxCacheSize    = "cacheSize"
	volumeCtxPollInterval = "pollIntervalSecs"
	volumeCtxMetadataTtl  = "metadataTtlMs"
	volumeCtxTokenKey     = "tokenKey"
	volumeCtxMountFlags   = "mountFlags"
	volumeCtxPodUID       = "csi.storage.k8s.io/pod.uid"
)

func (d *Driver) NodePublishVolume(_ context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	target := req.GetTargetPath()
	volCap := req.GetVolumeCapability()
	volCtx := req.GetVolumeContext()

	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volumeId is required")
	}
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "targetPath is required")
	}
	if volCap == nil {
		return nil, status.Error(codes.InvalidArgument, "volumeCapability is required")
	}
	if volCap.GetMount() == nil {
		return nil, status.Error(codes.InvalidArgument, "only mount access type is supported")
	}

	sourceType := volCtx[volumeCtxSourceType]
	sourceID := volCtx[volumeCtxSourceID]
	if sourceType == "" {
		return nil, status.Error(codes.InvalidArgument, "volumeContext must contain sourceType")
	}
	if sourceID == "" {
		return nil, status.Error(codes.InvalidArgument, "volumeContext must contain sourceId")
	}

	// Resolve token from secrets (empty for public repos without a secret).
	tokenKey := getWithDefault(volCtx, volumeCtxTokenKey, defaultTokenKey)
	token := req.GetSecrets()[tokenKey]

	workloadPodUID := volCtx[volumeCtxPodUID]
	willUseSidecar := d.sidecarMode && workloadPodUID != ""

	// Check existing mount state.
	mounted, err := d.mounter.IsMountPoint(target)
	if err != nil {
		if mount.IsCorruptedMnt(err) {
			if willUseSidecar {
				// In sidecar mode, the kernel FUSE mount intentionally has no
				// daemon until the sidecar connects. Treat as already mounted
				// (republish path will check health via error file).
				mounted = true
			} else {
				klog.Warningf("Stale mount detected at %s, force unmounting", target)
				if umountErr := d.mounter.Unmount(target); umountErr != nil {
					return nil, status.Errorf(codes.Internal, "failed to clean stale mount at %s: %v", target, umountErr)
				}
				mounted = false
			}
		} else if !os.IsNotExist(err) {
			return nil, status.Errorf(codes.Internal, "failed to check mount point %s: %v", target, err)
		}
	}

	if mounted {
		// Republish: kubelet calls us with fresh secrets.
		if token != "" {
			if willUseSidecar {
				refreshSidecarToken(workloadPodUID, volumeID, token)
			} else {
				if err := writeTokenFile(tokenFilePath(d.cacheBase, volumeID), token); err != nil {
					klog.Warningf("Failed to refresh token file for %s: %v", volumeID, err)
				}
			}
		}
		// Check mount health. In sidecar mode, read the error file from the
		// emptyDir. If the sidecar failed, unmount the stale FUSE mount so the
		// next NodePublishVolume call creates a fresh mount + socket for the
		// restarted sidecar. Return the error so kubelet emits FailedMount.
		if willUseSidecar {
			if err := checkSidecarHealth(workloadPodUID, volumeID); err != nil {
				// Unmount the stale FUSE mount. The next call will re-mount
				// with a new fd and socket for the restarted sidecar.
				_ = d.mounter.Unmount(target)
				return nil, status.Errorf(codes.Internal, "sidecar unhealthy for %s: %v", volumeID, err)
			}
		} else if err := d.mounter.CheckHealth(target); err != nil {
			return nil, status.Errorf(codes.Internal, "mount unhealthy for %s: %v", target, err)
		}
		return &csi.NodePublishVolumeResponse{}, nil
	}

	// Create target directory.
	if err := os.MkdirAll(target, 0750); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create target directory %s: %v", target, err)
	}

	// Extract volume mount group (fsGroup) from the CSI request.
	// Kubelet sends this when the pod has an fsGroup and the driver
	// advertises VOLUME_MOUNT_GROUP capability.
	volumeMountGroup := volCap.GetMount().GetVolumeMountGroup()

	// Build mount options.
	opts := MountOptions{
		Revision:         getWithDefault(volCtx, volumeCtxRevision, defaultRevision),
		HubEndpoint:      volCtx[volumeCtxHubEndpoint],
		CacheDir:         getWithDefault(volCtx, volumeCtxCacheDir, filepath.Join(d.cacheBase, sanitizeVolumeID(volumeID))),
		CacheSize:        volCtx[volumeCtxCacheSize],
		PollIntervalSecs: volCtx[volumeCtxPollInterval],
		MetadataTtlMs:    volCtx[volumeCtxMetadataTtl],
		ReadOnly:         req.GetReadonly(),
		WorkloadPodUID:   volCtx[volumeCtxPodUID],
		VolumeMountGroup: volumeMountGroup,
	}

	// When the pod specifies an fsGroup, pass --uid and --gid to hf-mount-fuse
	// so files appear owned by the pod's user. This makes volumes writable for
	// non-root containers (e.g. Docker Spaces with USER 1000).
	if volumeMountGroup != "" {
		opts.ExtraArgs = append(opts.ExtraArgs, "--uid="+volumeMountGroup, "--gid="+volumeMountGroup)
	}

	// If a token is provided, write it to a file for hf-mount to read.
	if token != "" {
		tokenFile := tokenFilePath(d.cacheBase, volumeID)
		if err := writeTokenFile(tokenFile, token); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to write token file: %v", err)
		}
		opts.TokenFile = tokenFile
	}

	// Pass mount flags straight through to hf-mount-fuse.
	for _, flag := range volCap.GetMount().GetMountFlags() {
		opts.ExtraArgs = append(opts.ExtraArgs, "--"+flag)
	}

	// Also accept comma-separated mount flags from volumeAttributes
	// (the only way to pass flags for inline ephemeral volumes).
	if raw := volCtx[volumeCtxMountFlags]; raw != "" {
		for _, flag := range strings.Split(raw, ",") {
			flag = strings.TrimSpace(flag)
			if flag != "" {
				opts.ExtraArgs = append(opts.ExtraArgs, "--"+flag)
			}
		}
	}

	// In sidecar mode, use fd-passing: open /dev/fuse, do the kernel mount,
	// and hand the fd to the sidecar via a Unix socket. Otherwise, fall back
	// to the pod-based mounter.
	if willUseSidecar {
		klog.Infof("Sidecar mode detected for pod %s, using fd-passing mount", opts.WorkloadPodUID)
		if err := sidecarMount(sourceType, sourceID, target, opts, volumeID); err != nil {
			return nil, status.Errorf(codes.Internal, "sidecar mount failed for %s %s at %s: %v", sourceType, sourceID, target, err)
		}
	} else {
		if err := d.mounter.Mount(sourceType, sourceID, target, opts); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to mount %s %s at %s: %v", sourceType, sourceID, target, err)
		}
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (d *Driver) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	target := req.GetTargetPath()

	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volumeId is required")
	}
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "targetPath is required")
	}

	// Always clean up the token file and sidecar socket on unpublish.
	defer d.cleanupTokenFile(volumeID)
	defer cleanupSidecarSocket(volumeID)

	mounted, err := d.mounter.IsMountPoint(target)
	if err != nil {
		if mount.IsCorruptedMnt(err) {
			klog.Warningf("Stale mount detected at %s, force unmounting", target)
			if umountErr := d.mounter.Unmount(target); umountErr != nil {
				return nil, status.Errorf(codes.Internal, "failed to unmount stale mount at %s: %v", target, umountErr)
			}
			return &csi.NodeUnpublishVolumeResponse{}, nil
		}
		if os.IsNotExist(err) {
			klog.V(4).Infof("Target %s does not exist, nothing to unmount", target)
			return &csi.NodeUnpublishVolumeResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal, "failed to check mount point %s: %v", target, err)
	}

	if !mounted {
		klog.V(4).Infof("Target %s is not mounted, cleaning up directory", target)
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return nil, status.Errorf(codes.Internal, "failed to remove target directory %s: %v", target, err)
		}
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}

	if err := d.mounter.Unmount(target); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to unmount %s: %v", target, err)
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (d *Driver) cleanupTokenFile(volumeID string) {
	tokenFile := tokenFilePath(d.cacheBase, volumeID)
	if err := os.Remove(tokenFile); err != nil && !os.IsNotExist(err) {
		klog.V(4).Infof("Remove token file %s: %v", tokenFile, err)
	}
	_ = os.Remove(filepath.Dir(tokenFile))
}

func (d *Driver) NodeStageVolume(_ context.Context, _ *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NodeStageVolume not supported")
}

func (d *Driver) NodeUnstageVolume(_ context.Context, _ *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NodeUnstageVolume not supported")
}

func (d *Driver) NodeGetVolumeStats(_ context.Context, _ *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NodeGetVolumeStats not supported")
}

func (d *Driver) NodeExpandVolume(_ context.Context, _ *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NodeExpandVolume not supported")
}

func (d *Driver) NodeGetCapabilities(_ context.Context, _ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_VOLUME_MOUNT_GROUP,
					},
				},
			},
		},
	}, nil
}

func (d *Driver) NodeGetInfo(_ context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId: d.nodeID,
	}, nil
}

// tokenFilePath returns the path where the CSI driver writes the token
// for hf-mount to re-read on refresh.
func tokenFilePath(cacheBase, volumeID string) string {
	return filepath.Join(cacheBase, sanitizeVolumeID(volumeID), "token")
}

func getWithDefault(m map[string]string, key, defaultVal string) string {
	if v, ok := m[key]; ok && v != "" {
		return v
	}
	return defaultVal
}

// sanitizeVolumeID encodes unsafe characters to prevent directory traversal and collisions.
func sanitizeVolumeID(id string) string {
	s := url.PathEscape(id)
	switch s {
	case ".":
		return "%2E"
	case "..":
		return "%2E%2E"
	default:
		return s
	}
}
