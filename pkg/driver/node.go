package driver

import (
	"context"
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
	defaultCacheBase = "/var/lib/hf-csi-driver/cache"
	defaultRevision  = "main"

	volumeCtxSourceType      = "sourceType"
	volumeCtxSourceID        = "sourceId"
	volumeCtxRevision        = "revision"
	volumeCtxHubEndpoint     = "hubEndpoint"
	volumeCtxCacheDir        = "cacheDir"
	volumeCtxCacheSize       = "cacheSize"
	volumeCtxPollInterval    = "pollIntervalSecs"
	volumeCtxMetadataTtl     = "metadataTtlMs"
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

	sourceType := volCtx[volumeCtxSourceType]
	sourceID := volCtx[volumeCtxSourceID]
	if sourceType == "" {
		return nil, status.Error(codes.InvalidArgument, "volumeContext must contain sourceType")
	}
	if sourceID == "" {
		return nil, status.Error(codes.InvalidArgument, "volumeContext must contain sourceId")
	}

	// Check existing mount state.
	mounted, err := d.mounter.IsMountPoint(target)
	if err != nil {
		if mount.IsCorruptedMnt(err) {
			klog.Warningf("Stale mount detected at %s, cleaning up", target)
			if umountErr := d.mounter.Unmount(target); umountErr != nil {
				return nil, status.Errorf(codes.Internal, "failed to clean stale mount at %s: %v", target, umountErr)
			}
			mounted = false
		} else if !os.IsNotExist(err) {
			return nil, status.Errorf(codes.Internal, "failed to check mount point %s: %v", target, err)
		}
	}

	if mounted {
		klog.Infof("Volume %s already mounted at %s", volumeID, target)
		return &csi.NodePublishVolumeResponse{}, nil
	}

	// Create target directory.
	if err := os.MkdirAll(target, 0750); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create target directory %s: %v", target, err)
	}

	// Build mount options.
	opts := MountOptions{
		Revision:         getWithDefault(volCtx, volumeCtxRevision, defaultRevision),
		HubEndpoint:      volCtx[volumeCtxHubEndpoint],
		CacheDir:         getWithDefault(volCtx, volumeCtxCacheDir, filepath.Join(defaultCacheBase, volumeID)),
		CacheSize:        volCtx[volumeCtxCacheSize],
		PollIntervalSecs: volCtx[volumeCtxPollInterval],
		MetadataTtlMs:    volCtx[volumeCtxMetadataTtl],
		ReadOnly:         req.GetReadonly(),
	}

	// Parse mount flags from PV mountOptions.
	if volCap.GetMount() != nil {
		for _, flag := range volCap.GetMount().GetMountFlags() {
			switch {
			case flag == "read-only":
				opts.ReadOnly = true
			case flag == "advanced-writes":
				opts.AdvancedWrites = true
			case strings.HasPrefix(flag, "uid="):
				opts.UID = strings.TrimPrefix(flag, "uid=")
			case strings.HasPrefix(flag, "gid="):
				opts.GID = strings.TrimPrefix(flag, "gid=")
			default:
				opts.ExtraArgs = append(opts.ExtraArgs, "--"+flag)
			}
		}
	}

	if err := d.mounter.Mount(sourceType, sourceID, target, opts); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to mount %s %s at %s: %v", sourceType, sourceID, target, err)
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
		os.Remove(target)
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}

	if err := d.mounter.Unmount(target); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to unmount %s: %v", target, err)
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
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
	return &csi.NodeGetCapabilitiesResponse{}, nil
}

func (d *Driver) NodeGetInfo(_ context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId: d.nodeID,
	}, nil
}

func getWithDefault(m map[string]string, key, defaultVal string) string {
	if v, ok := m[key]; ok && v != "" {
		return v
	}
	return defaultVal
}

