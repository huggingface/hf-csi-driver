package driver

import (
	"context"
	"fmt"

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
				"targets":      []interface{}{},
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

// addTarget adds a target path to the HFMount's spec.targets list.
func (c *hfMountClient) addTarget(ctx context.Context, name, target string) error {
	obj, err := c.resource().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	spec, _ := obj.Object["spec"].(map[string]interface{})
	targets, _ := spec["targets"].([]interface{})

	for _, t := range targets {
		if t == target {
			return nil // Already tracked.
		}
	}

	targets = append(targets, target)
	spec["targets"] = targets

	_, err = c.resource().Update(ctx, obj, metav1.UpdateOptions{})
	return err
}

// removeTarget removes a target path from the HFMount's spec.targets list.
// Returns the remaining target count.
func (c *hfMountClient) removeTarget(ctx context.Context, name, target string) (int, error) {
	obj, err := c.resource().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return 0, err
	}

	spec, _ := obj.Object["spec"].(map[string]interface{})
	targets, _ := spec["targets"].([]interface{})

	var remaining []interface{}
	for _, t := range targets {
		if t != target {
			remaining = append(remaining, t)
		}
	}

	spec["targets"] = remaining
	_, err = c.resource().Update(ctx, obj, metav1.UpdateOptions{})
	return len(remaining), err
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

// delete deletes the HFMount CR.
func (c *hfMountClient) delete(ctx context.Context, name string) error {
	return c.resource().Delete(ctx, name, metav1.DeleteOptions{})
}

// logCRDError logs CRD operation errors.
// CRD write errors are non-fatal; the in-memory binds map handles the hot path,
// but the CRD is the source of truth for mount args needed to recreate pods.
func logCRDError(op, name string, err error) {
	if err != nil {
		klog.Warningf("HFMount CRD %s %s: %v", op, name, err)
	}
}

// hfMountName generates a deterministic CRD name for a given mount.
func hfMountName(volumeID string) string {
	return fmt.Sprintf("hfm-%s", volumeID)
}
