package driver

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/klog/v2"
)

var hfMountGVR = schema.GroupVersionResource{
	Group:    "hf.csi.huggingface.co",
	Version:  "v1alpha1",
	Resource: "hfmounts",
}

// hfMountClient wraps the dynamic client for HFMount CRD operations.
type hfMountClient struct {
	client    dynamic.Interface
	namespace string
}

func newHFMountClient(client dynamic.Interface, namespace string) *hfMountClient {
	return &hfMountClient{client: client, namespace: namespace}
}

func (c *hfMountClient) resource() dynamic.ResourceInterface {
	return c.client.Resource(hfMountGVR).Namespace(c.namespace)
}

// create creates an HFMount CR for a given mount operation.
func (c *hfMountClient) create(ctx context.Context, name, nodeName, sourceType, sourceID, mountPodName, mountPath string, mountArgs []string) error {
	argsIface := make([]interface{}, len(mountArgs))
	for i, a := range mountArgs {
		argsIface[i] = a
	}

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "hf.csi.huggingface.co/v1alpha1",
			"kind":       "HFMount",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": c.namespace,
			},
			"spec": map[string]interface{}{
				"nodeName":     nodeName,
				"sourceType":   sourceType,
				"sourceID":     sourceID,
				"mountPodName": mountPodName,
				"mountPath":    mountPath,
				"mountArgs":    argsIface,
				"workloads":    []interface{}{},
			},
		},
	}

	_, err := c.resource().Create(ctx, obj, metav1.CreateOptions{})
	return err
}

// get retrieves an HFMount CR and returns its spec fields.
func (c *hfMountClient) get(ctx context.Context, name string) (map[string]interface{}, error) {
	obj, err := c.resource().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	spec, _ := obj.Object["spec"].(map[string]interface{})
	return spec, nil
}

// addWorkload adds a workload attachment (pod UID + target path + timestamp).
// Retries on conflict (409).
func (c *hfMountClient) addWorkload(ctx context.Context, name, podUID, targetPath string) error {
	return c.retryOnConflict(ctx, name, func(spec map[string]interface{}) (bool, error) {
		workloads, _ := spec["workloads"].([]interface{})

		for _, w := range workloads {
			wm, _ := w.(map[string]interface{})
			if wm["targetPath"] == targetPath {
				return false, nil // Already tracked.
			}
		}

		workloads = append(workloads, map[string]interface{}{
			"podUID":         podUID,
			"targetPath":     targetPath,
			"attachmentTime": time.Now().UTC().Format(time.RFC3339),
		})
		spec["workloads"] = workloads
		return true, nil
	})
}

// removeWorkload removes a workload by target path. Retries on conflict.
// Returns the remaining workload count.
func (c *hfMountClient) removeWorkload(ctx context.Context, name, targetPath string) (int, error) {
	var count int
	err := c.retryOnConflict(ctx, name, func(spec map[string]interface{}) (bool, error) {
		workloads, _ := spec["workloads"].([]interface{})

		var remaining []interface{}
		for _, w := range workloads {
			wm, _ := w.(map[string]interface{})
			if wm["targetPath"] != targetPath {
				remaining = append(remaining, w)
			}
		}

		count = len(remaining)
		if len(remaining) == len(workloads) {
			return false, nil // Nothing to remove.
		}
		spec["workloads"] = remaining
		return true, nil
	})
	return count, err
}

// list returns all HFMount CRs in the namespace filtered by node name.
func (c *hfMountClient) list(ctx context.Context, nodeName string) ([]unstructured.Unstructured, error) {
	list, err := c.resource().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var result []unstructured.Unstructured
	for _, item := range list.Items {
		spec, _ := item.Object["spec"].(map[string]interface{})
		if spec["nodeName"] == nodeName {
			result = append(result, item)
		}
	}
	return result, nil
}

// removeStaleWorkloads removes workloads whose pod UID is not in the given
// set of live pod UIDs (with a staleness threshold). Retries on conflict.
func (c *hfMountClient) removeStaleWorkloads(ctx context.Context, name string, livePodUIDs map[string]bool, staleThreshold time.Duration) (int, error) {
	var count int
	err := c.retryOnConflict(ctx, name, func(spec map[string]interface{}) (bool, error) {
		workloads, _ := spec["workloads"].([]interface{})

		now := time.Now().UTC()
		var remaining []interface{}
		for _, w := range workloads {
			wm, _ := w.(map[string]interface{})
			podUID, _ := wm["podUID"].(string)
			attachTimeStr, _ := wm["attachmentTime"].(string)

			exists := livePodUIDs[podUID]
			attachTime, _ := time.Parse(time.RFC3339, attachTimeStr)
			isStale := !attachTime.IsZero() && now.Sub(attachTime) > staleThreshold

			if exists || !isStale {
				remaining = append(remaining, w)
			} else {
				targetPath, _ := wm["targetPath"].(string)
				klog.Infof("Removing stale workload from CRD %s: podUID=%s target=%s (attached %s ago)", name, podUID, targetPath, now.Sub(attachTime).Round(time.Second))
			}
		}

		count = len(remaining)
		if len(remaining) == len(workloads) {
			return false, nil
		}
		spec["workloads"] = remaining
		return true, nil
	})
	return count, err
}

// retryOnConflict performs a read-modify-write loop on the CRD spec with
// retries on 409 Conflict errors. The mutate function receives the spec and
// returns (changed, error). If changed is false, the update is skipped.
func (c *hfMountClient) retryOnConflict(ctx context.Context, name string, mutate func(spec map[string]interface{}) (bool, error)) error {
	for attempt := 0; attempt < 5; attempt++ {
		obj, err := c.resource().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		spec, _ := obj.Object["spec"].(map[string]interface{})
		changed, mutErr := mutate(spec)
		if mutErr != nil {
			return mutErr
		}
		if !changed {
			return nil
		}

		_, err = c.resource().Update(ctx, obj, metav1.UpdateOptions{})
		if err == nil {
			return nil
		}
		if !errors.IsConflict(err) {
			return err
		}
		klog.V(4).Infof("HFMount CRD %s update conflict (attempt %d/5), retrying", name, attempt+1)
	}
	return fmt.Errorf("HFMount CRD %s: exceeded retry limit on conflict", name)
}

// updateStatus updates the HFMount's status.phase and status.message.
func (c *hfMountClient) updateStatus(ctx context.Context, name, phase, message string) error {
	obj, err := c.resource().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	status := map[string]interface{}{
		"phase":   phase,
		"message": message,
	}
	obj.Object["status"] = status

	_, err = c.resource().UpdateStatus(ctx, obj, metav1.UpdateOptions{})
	return err
}

// delete deletes the HFMount CR.
func (c *hfMountClient) delete(ctx context.Context, name string) error {
	err := c.resource().Delete(ctx, name, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

// logCRDError logs CRD operation errors.
// CRD write errors are non-fatal for addWorkload/removeWorkload/updateStatus;
// the CRD is the source of truth for mount args and stale-attachment cleanup.
func logCRDError(op, name string, err error) {
	if err != nil {
		klog.Warningf("HFMount CRD %s %s: %v", op, name, err)
	}
}

// hfMountName generates a deterministic CRD name for a given mount.
func hfMountName(volumeID string) string {
	return fmt.Sprintf("hfm-%s", volumeID)
}
