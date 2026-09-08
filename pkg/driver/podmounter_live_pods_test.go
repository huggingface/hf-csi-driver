package driver

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestListLivePodUIDsOnNode_ExcludesTerminalPods(t *testing.T) {
	pod := func(name string, phase corev1.PodPhase) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID("uid-" + name)},
			Spec:       corev1.PodSpec{NodeName: "node-a"},
			Status:     corev1.PodStatus{Phase: phase},
		}
	}
	client := fake.NewSimpleClientset(
		pod("pending", corev1.PodPending),
		pod("running", corev1.PodRunning),
		pod("failed", corev1.PodFailed),
		pod("succeeded", corev1.PodSucceeded),
	)
	m := &PodMounter{client: client, nodeID: "node-a"}

	live, err := m.listLivePodUIDsOnNode(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, uid := range []string{"uid-pending", "uid-running"} {
		if !live[uid] {
			t.Errorf("%s should be live", uid)
		}
	}
	for _, uid := range []string{"uid-failed", "uid-succeeded"} {
		if live[uid] {
			t.Errorf("%s is terminal and must not be live", uid)
		}
	}
}
