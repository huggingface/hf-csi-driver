package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// DefaultStartupTaintKey is the taint node bootstrap applies (kubelet registerWithTaints or
// the provisioner's node template) so nothing needing an HF volume is scheduled before the
// driver is registered with kubelet. Same pattern as ebs.csi.aws.com/agent-not-ready.
const DefaultStartupTaintKey = "hf.csi.huggingface.co/agent-not-ready"

const startupTaintPollInterval = 2 * time.Second

// RemoveStartupTaint blocks until kubelet lists DriverName in this node's CSINode (i.e.
// node-driver-registrar completed), then strips every taint with taintKey from the Node.
// It never returns an error before registration is observed: kubelet/registrar retry on
// their own and the taint must stay until they succeed. Returns nil once the taint is gone
// (or was never there), or ctx's error.
func RemoveStartupTaint(ctx context.Context, client kubernetes.Interface, nodeName, taintKey string) error {
	if err := waitForCSINodeDriver(ctx, client, nodeName); err != nil {
		return err
	}
	return removeNodeTaint(ctx, client, nodeName, taintKey)
}

func waitForCSINodeDriver(ctx context.Context, client kubernetes.Interface, nodeName string) error {
	ticker := time.NewTicker(startupTaintPollInterval)
	defer ticker.Stop()
	for {
		csiNode, err := client.StorageV1().CSINodes().Get(ctx, nodeName, metav1.GetOptions{})
		switch {
		case err == nil:
			for _, drv := range csiNode.Spec.Drivers {
				if drv.Name == DriverName {
					return nil
				}
			}
		case apierrors.IsNotFound(err):
			// CSINode is created by kubelet on first registration; keep polling.
		default:
			klog.V(4).Infof("Startup taint: CSINode %s not readable yet: %v", nodeName, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func removeNodeTaint(ctx context.Context, client kubernetes.Interface, nodeName, taintKey string) error {
	ticker := time.NewTicker(startupTaintPollInterval)
	defer ticker.Stop()
	for {
		err := tryRemoveNodeTaint(ctx, client, nodeName, taintKey)
		if err == nil {
			return nil
		}
		klog.Warningf("Startup taint: failed to remove %s from node %s, retrying: %v", taintKey, nodeName, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// tryRemoveNodeTaint does one read-modify-write of node.spec.taints. The test-and-set patch
// (JSON Patch `test` on the full taint list) makes a concurrent taint change fail with a
// conflict instead of being clobbered; the caller retries.
func tryRemoveNodeTaint(ctx context.Context, client kubernetes.Interface, nodeName, taintKey string) error {
	node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	kept := make([]corev1.Taint, 0, len(node.Spec.Taints))
	for _, taint := range node.Spec.Taints {
		if taint.Key != taintKey {
			kept = append(kept, taint)
		}
	}
	if len(kept) == len(node.Spec.Taints) {
		klog.Infof("Startup taint: %s not present on node %s", taintKey, nodeName)
		return nil
	}
	patch, err := json.Marshal([]map[string]interface{}{
		{"op": "test", "path": "/spec/taints", "value": node.Spec.Taints},
		{"op": "replace", "path": "/spec/taints", "value": kept},
	})
	if err != nil {
		return fmt.Errorf("marshal taint patch: %w", err)
	}
	if _, err := client.CoreV1().Nodes().Patch(ctx, nodeName, types.JSONPatchType, patch, metav1.PatchOptions{}); err != nil {
		return err
	}
	klog.Infof("Startup taint: removed %s from node %s", taintKey, nodeName)
	return nil
}
