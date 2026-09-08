package driver

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func taintedNode(name string, keys ...string) *corev1.Node {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	for _, key := range keys {
		node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{Key: key, Effect: corev1.TaintEffectNoSchedule})
	}
	return node
}

func csiNodeWith(name string, drivers ...string) *storagev1.CSINode {
	csiNode := &storagev1.CSINode{ObjectMeta: metav1.ObjectMeta{Name: name}}
	for _, drv := range drivers {
		csiNode.Spec.Drivers = append(csiNode.Spec.Drivers, storagev1.CSINodeDriver{Name: drv, NodeID: name})
	}
	return csiNode
}

func nodeTaintKeys(t *testing.T, client *fake.Clientset, name string) []string {
	t.Helper()
	node, err := client.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	keys := make([]string, 0, len(node.Spec.Taints))
	for _, taint := range node.Spec.Taints {
		keys = append(keys, taint.Key)
	}
	return keys
}

func TestTryRemoveNodeTaint_KeepsOtherTaints(t *testing.T) {
	client := fake.NewSimpleClientset(taintedNode("n1", "nvidia.com/gpu", DefaultStartupTaintKey))
	if err := tryRemoveNodeTaint(context.Background(), client, "n1", DefaultStartupTaintKey); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	keys := nodeTaintKeys(t, client, "n1")
	if len(keys) != 1 || keys[0] != "nvidia.com/gpu" {
		t.Fatalf("expected only nvidia.com/gpu to remain, got %v", keys)
	}
}

func TestTryRemoveNodeTaint_NoopWhenAbsent(t *testing.T) {
	client := fake.NewSimpleClientset(taintedNode("n1", "nvidia.com/gpu"))
	if err := tryRemoveNodeTaint(context.Background(), client, "n1", DefaultStartupTaintKey); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(client.Actions()) != 1 {
		t.Fatalf("expected a single get and no patch, got %d actions", len(client.Actions()))
	}
}

func TestRemoveStartupTaint_WaitsForDriverRegistration(t *testing.T) {
	client := fake.NewSimpleClientset(taintedNode("n1", DefaultStartupTaintKey), csiNodeWith("n1", "ebs.csi.aws.com"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- RemoveStartupTaint(ctx, client, "n1", DefaultStartupTaintKey) }()

	// Not registered yet: the taint must stay.
	time.Sleep(3 * startupTaintPollInterval / 2)
	if keys := nodeTaintKeys(t, client, "n1"); len(keys) != 1 {
		t.Fatalf("taint removed before driver registration: %v", keys)
	}

	// Registrar completes: kubelet adds our driver to the CSINode.
	if _, err := client.StorageV1().CSINodes().Update(ctx, csiNodeWith("n1", "ebs.csi.aws.com", DriverName), metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update csinode: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("RemoveStartupTaint: %v", err)
	}
	if keys := nodeTaintKeys(t, client, "n1"); len(keys) != 0 {
		t.Fatalf("taint still present after registration: %v", keys)
	}
}

func TestRemoveStartupTaint_HonoursContext(t *testing.T) {
	client := fake.NewSimpleClientset(taintedNode("n1", DefaultStartupTaintKey))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := RemoveStartupTaint(ctx, client, "n1", DefaultStartupTaintKey); err == nil {
		t.Fatal("expected context error when the driver never registers")
	}
}
