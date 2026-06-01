package driver

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIsPodTerminal(t *testing.T) {
	tests := []struct {
		phase    corev1.PodPhase
		terminal bool
	}{
		{corev1.PodPending, false},
		{corev1.PodRunning, false},
		{corev1.PodSucceeded, true},
		{corev1.PodFailed, true},
		{corev1.PodUnknown, false},
	}
	for _, tt := range tests {
		pod := &corev1.Pod{Status: corev1.PodStatus{Phase: tt.phase}}
		if got := isPodTerminal(pod); got != tt.terminal {
			t.Errorf("isPodTerminal(%s) = %v, want %v", tt.phase, got, tt.terminal)
		}
	}
}

func TestTotalRestartCount(t *testing.T) {
	tests := []struct {
		name     string
		statuses []corev1.ContainerStatus
		want     int32
	}{
		{"no containers", nil, 0},
		{"single container no restarts", []corev1.ContainerStatus{{RestartCount: 0}}, 0},
		{"single container with restarts", []corev1.ContainerStatus{{RestartCount: 3}}, 3},
		{"multiple containers", []corev1.ContainerStatus{{RestartCount: 2}, {RestartCount: 5}}, 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: tt.statuses}}
			if got := totalRestartCount(pod); got != tt.want {
				t.Errorf("totalRestartCount() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestMountID(t *testing.T) {
	// Deterministic: same input -> same output.
	id1 := mountID("/var/lib/kubelet/pods/abc/volumes/csi/vol1/mount")
	id2 := mountID("/var/lib/kubelet/pods/abc/volumes/csi/vol1/mount")
	if id1 != id2 {
		t.Errorf("mountID not deterministic: %q != %q", id1, id2)
	}
	// Different inputs -> different outputs.
	id3 := mountID("/var/lib/kubelet/pods/abc/volumes/csi/vol2/mount")
	if id1 == id3 {
		t.Errorf("mountID collision: %q == %q", id1, id3)
	}
	// Length: 12 hex chars (6 bytes).
	if len(id1) != 12 {
		t.Errorf("mountID length = %d, want 12", len(id1))
	}
}

func TestMountIDFromSource(t *testing.T) {
	got := mountIDFromSource("/var/lib/hf-csi-driver/mnt/abc123")
	if got != "abc123" {
		t.Errorf("mountIDFromSource() = %q, want %q", got, "abc123")
	}
}

func TestSanitizeLabelValue(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"my-node", "my-node"},
		{"node.example.com", "node.example.com"},
		{"ip-10-0-1-5.ec2.internal", "ip-10-0-1-5.ec2.internal"},
		// Invalid chars replaced with underscore.
		{"node/with/slashes", "node_with_slashes"},
		{"node@special#chars", "node_special_chars"},
		// Leading/trailing non-alphanumeric trimmed.
		{"_leading", "leading"},
		{"trailing_", "trailing"},
		{"__both__", "both"},
		{".dotstart", "dotstart"},
		{"dotend.", "dotend"},
		// Truncated to 63 chars.
		{"a" + string(make([]byte, 100)), "a"},
		// Empty input.
		{"", ""},
		// All invalid chars -> empty after trim.
		{"___", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeLabelValue(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeLabelValue(%q) = %q, want %q", tt.input, got, tt.want)
			}
			// Verify result is valid K8s label value.
			if len(got) > 63 {
				t.Errorf("result too long: %d", len(got))
			}
			if len(got) > 0 {
				if !isAlphanumeric(rune(got[0])) {
					t.Errorf("result starts with non-alphanumeric: %q", got)
				}
				if !isAlphanumeric(rune(got[len(got)-1])) {
					t.Errorf("result ends with non-alphanumeric: %q", got)
				}
			}
		})
	}
}

func TestSanitizeLabelValue_LongInput(t *testing.T) {
	long := make([]byte, 200)
	for i := range long {
		long[i] = 'a'
	}
	got := sanitizeLabelValue(string(long))
	if len(got) > 63 {
		t.Errorf("sanitizeLabelValue did not truncate: len=%d", len(got))
	}
}

func TestHfMountName(t *testing.T) {
	got := hfMountName("abc123")
	if got != "hfm-abc123" {
		t.Errorf("hfMountName() = %q, want %q", got, "hfm-abc123")
	}
}

func TestIsAlphanumeric(t *testing.T) {
	alphanumeric := "abcxyzABCXYZ0189"
	for _, r := range alphanumeric {
		if !isAlphanumeric(r) {
			t.Errorf("isAlphanumeric(%c) = false, want true", r)
		}
	}
	nonAlphanumeric := "._-/@# "
	for _, r := range nonAlphanumeric {
		if isAlphanumeric(r) {
			t.Errorf("isAlphanumeric(%c) = true, want false", r)
		}
	}
}

func TestBuildMountPodLabels(t *testing.T) {
	// Verify the pod template has the expected labels and annotations.
	m := &PodMounter{
		namespace:       "default",
		nodeID:          "test-node",
		image:           "test:latest",
		imagePullPolicy: corev1.PullIfNotPresent,
		serviceAccount:  "sa",
		cacheDir:        "/cache",
	}
	pod := m.buildMountPod("hf-mount-abc", "abc", "repo", "user/model", "/mnt/abc", []string{"repo", "user/model", "/mnt/hf/abc"}, MountResources{})

	if pod.Labels[labelApp] != labelAppValue {
		t.Errorf("label %s = %q, want %q", labelApp, pod.Labels[labelApp], labelAppValue)
	}
	if pod.Labels[labelNode] != "test-node" {
		t.Errorf("label %s = %q, want %q", labelNode, pod.Labels[labelNode], "test-node")
	}
	if pod.Annotations[annotSourceType] != "repo" {
		t.Errorf("annotation %s = %q, want %q", annotSourceType, pod.Annotations[annotSourceType], "repo")
	}
	if pod.Annotations[annotSourceID] != "user/model" {
		t.Errorf("annotation %s = %q, want %q", annotSourceID, pod.Annotations[annotSourceID], "user/model")
	}
	if pod.Annotations[annotMountPath] != "/mnt/abc" {
		t.Errorf("annotation %s = %q, want %q", annotMountPath, pod.Annotations[annotMountPath], "/mnt/abc")
	}
	// Verify env vars are set.
	container := pod.Spec.Containers[0]
	envMap := make(map[string]string)
	for _, env := range container.Env {
		envMap[env.Name] = env.Value
	}
	if envMap["HF_CSI_SOURCE_TYPE"] != "repo" {
		t.Errorf("env HF_CSI_SOURCE_TYPE = %q, want %q", envMap["HF_CSI_SOURCE_TYPE"], "repo")
	}
	if envMap["HF_CSI_NODE"] != "test-node" {
		t.Errorf("env HF_CSI_NODE = %q, want %q", envMap["HF_CSI_NODE"], "test-node")
	}
	// Verify privileged.
	if pod.Spec.Containers[0].SecurityContext == nil || *pod.Spec.Containers[0].SecurityContext.Privileged != true {
		t.Error("mount pod container should be privileged")
	}
	// Verify restart policy.
	if pod.Spec.RestartPolicy != corev1.RestartPolicyOnFailure {
		t.Errorf("restart policy = %q, want OnFailure", pod.Spec.RestartPolicy)
	}
}

func TestBuildMountPodProxyEnv(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("HTTP_PROXY", "http://proxy.example:8080")
	t.Setenv("NO_PROXY", "localhost,.svc")

	m := &PodMounter{
		namespace:       "default",
		nodeID:          "test-node",
		image:           "test:latest",
		imagePullPolicy: corev1.PullIfNotPresent,
		serviceAccount:  "sa",
		cacheDir:        "/cache",
	}
	pod := m.buildMountPod("hf-mount-abc", "abc", "repo", "user/model", "/mnt/abc", []string{"repo", "user/model", "/mnt/hf/abc"}, MountResources{})

	envMap := make(map[string]string)
	for _, env := range pod.Spec.Containers[0].Env {
		envMap[env.Name] = env.Value
	}
	if envMap["HTTP_PROXY"] != "http://proxy.example:8080" {
		t.Errorf("env HTTP_PROXY = %q", envMap["HTTP_PROXY"])
	}
	if envMap["NO_PROXY"] != "localhost,.svc" {
		t.Errorf("env NO_PROXY = %q", envMap["NO_PROXY"])
	}
}

func TestBuildMountPodNodeAffinity(t *testing.T) {
	m := &PodMounter{
		namespace:       "default",
		nodeID:          "specific-node",
		image:           "test:latest",
		imagePullPolicy: corev1.PullAlways,
		serviceAccount:  "sa",
		cacheDir:        "/cache",
	}
	pod := m.buildMountPod("hf-mount-abc", "abc", "bucket", "user/b", "/mnt/abc", []string{"bucket", "user/b", "/mnt/hf/abc"}, MountResources{})

	affinity := pod.Spec.Affinity
	if affinity == nil || affinity.NodeAffinity == nil {
		t.Fatal("missing node affinity")
	}
	terms := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 {
		t.Fatalf("expected 1 node selector term, got %d", len(terms))
	}
	fields := terms[0].MatchFields
	if len(fields) != 1 {
		t.Fatalf("expected 1 match field, got %d", len(fields))
	}
	if fields[0].Key != "metadata.name" || fields[0].Values[0] != "specific-node" {
		t.Errorf("node affinity field = %+v, want metadata.name=specific-node", fields[0])
	}
}

func TestBuildMountPodImagePullSecrets(t *testing.T) {
	m := &PodMounter{
		namespace:       "default",
		nodeID:          "node",
		image:           "test:latest",
		imagePullPolicy: corev1.PullIfNotPresent,
		imagePullSecrets: []corev1.LocalObjectReference{
			{Name: "secret1"},
			{Name: "secret2"},
		},
		serviceAccount: "sa",
		cacheDir:       "/cache",
	}
	pod := m.buildMountPod("hf-mount-abc", "abc", "repo", "user/m", "/mnt/abc", []string{"repo", "user/m", "/mnt/hf/abc"}, MountResources{})

	if len(pod.Spec.ImagePullSecrets) != 2 {
		t.Fatalf("expected 2 pull secrets, got %d", len(pod.Spec.ImagePullSecrets))
	}
	if pod.Spec.ImagePullSecrets[0].Name != "secret1" {
		t.Errorf("pull secret 0 = %q, want %q", pod.Spec.ImagePullSecrets[0].Name, "secret1")
	}
}

func TestBuildMountPodServiceAccount(t *testing.T) {
	m := &PodMounter{
		namespace:       "default",
		nodeID:          "node",
		image:           "test:latest",
		imagePullPolicy: corev1.PullIfNotPresent,
		serviceAccount:  "custom-sa",
		cacheDir:        "/cache",
	}
	pod := m.buildMountPod("hf-mount-abc", "abc", "repo", "user/m", "/mnt/abc", nil, MountResources{})

	if pod.Spec.ServiceAccountName != "custom-sa" {
		t.Errorf("service account = %q, want %q", pod.Spec.ServiceAccountName, "custom-sa")
	}
}

// Verify tolerations allow scheduling on any node.
func TestBuildMountPodTolerations(t *testing.T) {
	m := &PodMounter{
		namespace:       "default",
		nodeID:          "node",
		image:           "test:latest",
		imagePullPolicy: corev1.PullIfNotPresent,
		serviceAccount:  "sa",
		cacheDir:        "/cache",
	}
	pod := m.buildMountPod("hf-mount-abc", "abc", "repo", "user/m", "/mnt/abc", nil, MountResources{})

	if len(pod.Spec.Tolerations) != 1 {
		t.Fatalf("expected 1 toleration, got %d", len(pod.Spec.Tolerations))
	}
	if pod.Spec.Tolerations[0].Operator != corev1.TolerationOpExists {
		t.Errorf("toleration operator = %q, want Exists", pod.Spec.Tolerations[0].Operator)
	}
}

// Verify node ID sanitization for labels.
func TestSanitizeLabelValue_NodeIDs(t *testing.T) {
	tests := []struct {
		nodeID string
		valid  bool // result should be non-empty
	}{
		{"ip-10-0-1-5.ec2.internal", true},
		{"gke-cluster-pool-abc123", true},
		{"aks-nodepool1-12345678-vmss000000", true},
		{"node-with-63-chars-" + "a]b[c{d}e(f)g*h+i?j\\k/l|m^n$o.p", true},
	}
	for _, tt := range tests {
		got := sanitizeLabelValue(tt.nodeID)
		if tt.valid && got == "" {
			t.Errorf("sanitizeLabelValue(%q) = empty, want non-empty", tt.nodeID)
		}
	}
}

// Verify that DeletionTimestamp makes pods appear stale alongside isPodTerminal.
func TestPodStaleConditions(t *testing.T) {
	now := metav1.Now()
	tests := []struct {
		name         string
		phase        corev1.PodPhase
		deleting     bool
		wantTerminal bool
	}{
		{"running", corev1.PodRunning, false, false},
		{"running+deleting", corev1.PodRunning, true, false},
		{"failed", corev1.PodFailed, false, true},
		{"succeeded", corev1.PodSucceeded, false, true},
		{"failed+deleting", corev1.PodFailed, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{Status: corev1.PodStatus{Phase: tt.phase}}
			if tt.deleting {
				pod.DeletionTimestamp = &now
			}
			if got := isPodTerminal(pod); got != tt.wantTerminal {
				t.Errorf("isPodTerminal() = %v, want %v", got, tt.wantTerminal)
			}
		})
	}
}
