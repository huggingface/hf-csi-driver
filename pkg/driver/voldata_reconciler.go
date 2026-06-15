package driver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	// DefaultVolDataReconcileInterval is how often the reconciler sweeps.
	DefaultVolDataReconcileInterval = 2 * time.Minute

	// volDataStaleThreshold: only repair a volume dir whose vol_data.json has
	// been missing at least this long. A healthy mount gets vol_data.json
	// written within milliseconds, so this avoids racing an in-progress
	// NodePublishVolume for a volume that is legitimately still being set up.
	volDataStaleThreshold = 2 * time.Minute
)

// OwnedVolumeLister returns the CSI volumes on this node that are genuinely
// backed by THIS driver, as podUID -> set(volumeName). The reconciler only ever
// touches directories present in this set, so it can never write metadata for a
// foreign driver's volume or a volume whose ownership it cannot prove.
type OwnedVolumeLister func() (map[string]map[string]bool, error)

// NewNodeOwnedVolumeLister lists pods scheduled on nodeName and collects, from
// their *live specs*, the inline CSI volumes whose driver is ours. Ownership is
// taken from the authoritative pod spec — never inferred from the on-disk
// directory name (which is a pod-spec-chosen volume name the driver does not
// control and a foreign CSI driver could collide with).
//
// Pods stuck Terminating still exist in the API, so their volumes are still
// listed here; pods already gone are intentionally absent and left untouched.
func NewNodeOwnedVolumeLister(client kubernetes.Interface, nodeName string) OwnedVolumeLister {
	return func() (map[string]map[string]bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		pods, err := client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		if err != nil {
			return nil, err
		}
		owned := make(map[string]map[string]bool)
		for i := range pods.Items {
			p := &pods.Items[i]
			for _, v := range p.Spec.Volumes {
				// Inline CSI volumes only. PVC-backed volumes are intentionally
				// out of scope: on disk they are named by PV name (not the pod
				// volume name) and carry the driver on the PV, not the pod spec;
				// the observed mid-init wedge is the inline-ephemeral case.
				if v.CSI == nil || v.CSI.Driver != DriverName {
					continue
				}
				uid := string(p.UID)
				if owned[uid] == nil {
					owned[uid] = make(map[string]bool)
				}
				owned[uid][v.Name] = true
			}
		}
		return owned, nil
	}
}

// StartVolDataReconciler periodically repairs CSI volume directories of our
// volumes that are missing kubelet's vol_data.json, then blocks until stopCh
// closes.
//
// Background: a pod deleted while still initializing can leave an empty
// `<kubeletRoot>/pods/<uid>/volumes/kubernetes.io~csi/<vol>/` directory with no
// vol_data.json. kubelet's volume reconciler then retries UnmountVolume forever
// — it fails in NewUnmounter reading the missing vol_data.json, BEFORE any CSI
// call — so the volume is never marked clean and the pod is stuck Terminating
// (grace never reaches 0). Removing the dir does NOT help a running kubelet (the
// volume persists in its in-memory actualStateOfWorld); only making NewUnmounter
// succeed does. Writing a minimal valid vol_data.json lets kubelet proceed to
// our (idempotent) NodeUnpublishVolume and finalize the pod — without a kubelet
// restart or a manual force-delete.
func StartVolDataReconciler(kubeletRoot, nodeID string, interval time.Duration, lister OwnedVolumeLister, stopCh <-chan struct{}) {
	if interval <= 0 {
		interval = DefaultVolDataReconcileInterval
	}
	podsDir := filepath.Join(kubeletRoot, "pods")
	klog.Infof("vol_data reconciler started (pods dir=%s, interval=%s)", podsDir, interval)

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		reconcileVolData(podsDir, nodeID, lister)
		select {
		case <-stopCh:
			klog.Info("vol_data reconciler stopping")
			return
		case <-t.C:
		}
	}
}

func reconcileVolData(podsDir, nodeID string, lister OwnedVolumeLister) {
	owned, err := lister()
	if err != nil {
		// Without an authoritative ownership list we do nothing — never guess.
		klog.V(4).Infof("vol_data reconciler: list owned volumes: %v", err)
		return
	}
	repaired := 0
	for uid, vols := range owned {
		for volName := range vols {
			volDir := filepath.Join(podsDir, uid, "volumes", "kubernetes.io~csi", volName)
			if repairVolData(volDir, volName, nodeID) {
				repaired++
			}
		}
	}
	if repaired > 0 {
		klog.Infof("vol_data reconciler: repaired %d stuck volume dir(s) this sweep", repaired)
	}
}

// repairVolData writes a minimal vol_data.json into volDir when it exists, is
// missing vol_data.json, and is older than volDataStaleThreshold. Returns true
// if it wrote one. Caller must have already confirmed volDir belongs to us.
func repairVolData(volDir, volName, nodeID string) bool {
	dataPath := filepath.Join(volDir, "vol_data.json")

	// Lstat (not Stat) so a symlinked vol_data.json or volDir is never followed.
	if _, err := os.Lstat(dataPath); err == nil {
		return false // already present — healthy
	} else if !os.IsNotExist(err) {
		klog.V(4).Infof("vol_data reconciler: lstat %s: %v", dataPath, err)
		return false
	}

	info, err := os.Lstat(volDir)
	if err != nil || !info.IsDir() {
		return false // gone, or not a real directory (e.g. symlink) — skip
	}
	if time.Since(info.ModTime()) < volDataStaleThreshold {
		return false // too fresh — may be an in-progress publish, don't race kubelet
	}

	// Mirrors the on-disk schema kubelet writes. NewUnmounter only needs
	// driverName + volumeHandle to construct the unmounter; the volumeHandle
	// value is irrelevant here because our NodeUnpublishVolume keys cleanup on
	// the target path and is idempotent. specVolID must match the volume name.
	data := map[string]string{
		"driverName":          DriverName,
		"specVolID":           volName,
		"volumeHandle":        "recovered-" + volName,
		"nodeName":            nodeID,
		"volumeLifecycleMode": "Ephemeral",
		"attachmentID":        "",
	}
	buf, err := json.Marshal(data)
	if err != nil {
		return false
	}

	// O_CREATE|O_EXCL: if kubelet (or anyone) writes the real vol_data.json
	// between our Lstat and here, the create fails with EEXIST and we leave the
	// real file untouched. O_EXCL also refuses to follow a pre-existing symlink.
	f, err := os.OpenFile(dataPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return false // lost the race to the real writer — benign
		}
		klog.Warningf("vol_data reconciler: create %s: %v", dataPath, err)
		return false
	}
	defer f.Close()
	if _, err := f.Write(buf); err != nil {
		klog.Warningf("vol_data reconciler: write %s: %v", dataPath, err)
		return false
	}
	klog.Infof("vol_data reconciler: wrote recovery vol_data.json for stuck volume %q (%s)", volName, volDir)
	return true
}
