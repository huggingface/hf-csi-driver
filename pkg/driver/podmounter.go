package driver

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
	"k8s.io/utils/ptr"
)

const (
	mountPodPrefix   = "hf-mount-"
	labelApp         = "hf.csi.huggingface.co/app"
	labelAppValue    = "hf-mount"
	labelVolumeID    = "hf.csi.huggingface.co/volume-id"
	labelNode        = "hf.csi.huggingface.co/node"
	annotSourceType  = "hf.csi.huggingface.co/source-type"
	annotSourceID    = "hf.csi.huggingface.co/source-id"
	annotMountPath   = "hf.csi.huggingface.co/mount-path"
	mountBaseDir     = "/var/lib/hf-csi-driver/mnt"
	podReadyTimeout  = 2 * time.Minute
	podReadyPoll     = time.Second
	mountReadyPollPM = 500 * time.Millisecond
	mountTimeoutPM   = 60 * time.Second
)

// PodMounter implements Mounter by delegating FUSE mounts to dedicated Kubernetes pods.
// Mount pods survive CSI driver restarts, avoiding I/O errors for workloads.
//
// A pod informer watches mount pods and reacts to lifecycle events:
//   - Pod restarts (container restartCount change): re-bind stale targets
//   - Pod deletion: clean up stale bind mounts (after references drain)
type PodMounter struct {
	mu               sync.Mutex
	client           kubernetes.Interface
	namespace        string
	nodeID           string
	image            string
	imagePullPolicy  corev1.PullPolicy
	imagePullSecrets []corev1.LocalObjectReference
	cacheDir         string
	checker          mount.Interface
	crd              *hfMountClient

	// binds tracks target -> source mount path for bind-mounted volumes.
	binds map[string]string

	// getMountRefs returns all mount references to a path.
	getMountRefs func(pathname string) ([]string, error)

	// sourceLocks provides per-source ref-counted mutexes to serialize
	// mount/unmount/rebind operations on the same FUSE source.
	sourceLocks map[string]*refMutex
}

func NewPodMounter(client kubernetes.Interface, dynClient dynamic.Interface, namespace, nodeID, image string, pullPolicy corev1.PullPolicy, pullSecrets []corev1.LocalObjectReference, cacheDir string) *PodMounter {
	checker := mount.New("")
	return &PodMounter{
		client:           client,
		namespace:        namespace,
		nodeID:           nodeID,
		image:            image,
		imagePullPolicy:  pullPolicy,
		imagePullSecrets: pullSecrets,
		cacheDir:         cacheDir,
		checker:         checker,
		crd:             newHFMountClient(dynClient, namespace),
		binds:           make(map[string]string),
		sourceLocks:     make(map[string]*refMutex),
		getMountRefs:    checker.GetMountRefs,
	}
}

// acquireSourceLock returns a per-source mutex with its refcount incremented.
func (m *PodMounter) acquireSourceLock(source string) *refMutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	lk, ok := m.sourceLocks[source]
	if !ok {
		lk = &refMutex{}
		m.sourceLocks[source] = lk
	}
	lk.refs++
	return lk
}

// releaseSourceLock decrements the refcount and cleans up if zero.
func (m *PodMounter) releaseSourceLock(source string, lk *refMutex) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lk.refs--
	if lk.refs == 0 {
		delete(m.sourceLocks, source)
	}
}

// Start launches a pod informer that watches mount pods on this node.
func (m *PodMounter) Start(stopCh <-chan struct{}) {
	factory := informers.NewSharedInformerFactoryWithOptions(
		m.client,
		30*time.Second,
		informers.WithNamespace(m.namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = fmt.Sprintf("%s=%s,%s=%s", labelApp, labelAppValue, labelNode, sanitizeLabelValue(m.nodeID))
		}),
	)

	podInformer := factory.Core().V1().Pods().Informer()
	_, _ = podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				return
			}
			// Catch pods that Recover() may have skipped (mount not ready at startup).
			mountPath := pod.Annotations[annotMountPath]
			if mountPath == "" || pod.Status.Phase != corev1.PodRunning {
				return
			}
			m.mu.Lock()
			hasBinds := false
			for _, source := range m.binds {
				if source == mountPath {
					hasBinds = true
					break
				}
			}
			m.mu.Unlock()
			if !hasBinds {
				klog.Infof("Informer add: pod %s has no tracked binds, attempting late adoption for %s", pod.Name, mountPath)
				go m.lateAdopt(mountPath)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPod, ok1 := oldObj.(*corev1.Pod)
			newPod, ok2 := newObj.(*corev1.Pod)
			if !ok1 || !ok2 {
				return
			}
			m.handlePodUpdate(oldPod, newPod)
		},
		DeleteFunc: func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					return
				}
				pod, ok = tombstone.Obj.(*corev1.Pod)
				if !ok {
					return
				}
			}
			m.handlePodDelete(pod)
		},
	})

	factory.Start(stopCh)
	factory.WaitForCacheSync(stopCh)
	klog.Infof("Mount pod watcher started for node %s", m.nodeID)

	// Periodic cleanup: scan for orphaned source mounts whose pods are gone.
	go m.periodicCleanup(stopCh)
}

// periodicCleanup runs every 2 minutes to find and clean up orphaned mounts.
// This catches cases missed by event handlers (e.g. pod deleted while refs
// were still present, or events missed during driver restart).
func (m *PodMounter) periodicCleanup(stopCh <-chan struct{}) {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			m.runCleanupScan()
		}
	}
}

func (m *PodMounter) runCleanupScan() {
	entries, err := os.ReadDir(mountBaseDir)
	if err != nil {
		return
	}

	ctx := context.TODO()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		mountPath := filepath.Join(mountBaseDir, entry.Name())
		podName := mountPodPrefix + entry.Name()

		mounted, mountErr := m.checker.IsMountPoint(mountPath)
		corrupted := mountErr != nil && mount.IsCorruptedMnt(mountErr)
		if mountErr != nil && !corrupted {
			continue // Unexpected error, skip.
		}
		if !mounted && !corrupted {
			continue // Not a mount point, skip.
		}

		// Check if the pod still exists and its phase.
		pod, getErr := m.client.CoreV1().Pods(m.namespace).Get(ctx, podName, metav1.GetOptions{})
		if getErr != nil && !errors.IsNotFound(getErr) {
			continue // API error, skip.
		}

		podGone := errors.IsNotFound(getErr)
		podTerminal := !podGone && pod != nil &&
			(pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded)

		if !podGone && !podTerminal {
			continue // Pod exists and is healthy.
		}

		if podGone {
			klog.Infof("Periodic cleanup: orphaned mount at %s (pod %s gone)", mountPath, podName)
		} else {
			klog.Infof("Periodic cleanup: mount at %s with terminal pod %s (%s)", mountPath, podName, pod.Status.Phase)
		}
		m.cleanupSource(mountPath)
	}
}

// handlePodUpdate reacts to mount pod changes.
// Detects both phase transitions AND container restarts (which keep phase=Running).
func (m *PodMounter) handlePodUpdate(oldPod, newPod *corev1.Pod) {
	mountPath := newPod.Annotations[annotMountPath]
	if mountPath == "" {
		return
	}

	phaseChanged := oldPod.Status.Phase != newPod.Status.Phase
	restarted := totalRestartCount(newPod) > totalRestartCount(oldPod)

	if newPod.Status.Phase == corev1.PodRunning && (phaseChanged || restarted) {
		if restarted {
			klog.Infof("Mount pod %s container restarted (restarts: %d -> %d), re-checking binds for %s",
				newPod.Name, totalRestartCount(oldPod), totalRestartCount(newPod), mountPath)
		} else {
			klog.Infof("Mount pod %s is now Running, checking binds for %s", newPod.Name, mountPath)
		}
		go m.rebindTargets(mountPath)
	}

	if phaseChanged && (newPod.Status.Phase == corev1.PodFailed || newPod.Status.Phase == corev1.PodSucceeded) {
		klog.Warningf("Mount pod %s entered terminal phase %s, cleaning up", newPod.Name, newPod.Status.Phase)
		go m.cleanupSource(mountPath)
	}
}

// handlePodDelete reacts to mount pod deletion.
// Waits for workload references to drain before unmounting.
func (m *PodMounter) handlePodDelete(pod *corev1.Pod) {
	mountPath := pod.Annotations[annotMountPath]
	if mountPath == "" {
		return
	}

	klog.Warningf("Mount pod %s deleted, checking references for %s", pod.Name, mountPath)

	lk := m.acquireSourceLock(mountPath)
	lk.Lock()
	defer func() {
		lk.Unlock()
		m.releaseSourceLock(mountPath, lk)
	}()

	mountRefs, refErr := m.getMountRefs(mountPath)
	if refErr != nil {
		klog.Warningf("Cannot enumerate mount refs for %s, deferring cleanup: %v", mountPath, refErr)
		return
	}
	refs := len(mountRefs)
	if refs > 0 {
		klog.Infof("Source %s still has %d active bind mount references, deferring cleanup to NodeUnpublishVolume", mountPath, refs)
		return
	}

	// No active references: safe to cleanup.
	klog.Infof("No active references for %s, cleaning up", mountPath)
	if err := fuseUnmount(mountPath); err != nil {
		klog.V(4).Infof("Cleanup unmount %s: %v", mountPath, err)
	}

	m.mu.Lock()
	var staleTargets []string
	for target, source := range m.binds {
		if source == mountPath {
			staleTargets = append(staleTargets, target)
		}
	}
	for _, target := range staleTargets {
		delete(m.binds, target)
	}
	m.mu.Unlock()

	for _, target := range staleTargets {
		klog.Warningf("Cleaning stale bind mount at %s (source pod deleted)", target)
		_ = fuseUnmount(target)
	}
}

// lateAdopt handles pods that Recover() may have skipped (e.g. mount not
// ready at startup). When the informer first sees them running, we try to
// rebuild binds from mountinfo.
func (m *PodMounter) lateAdopt(mountPath string) {
	// Wait briefly for mount to appear.
	for i := 0; i < 15; i++ {
		mounted, err := m.checker.IsMountPoint(mountPath)
		if err == nil && mounted {
			break
		}
		time.Sleep(time.Second)
	}

	mounted, _ := m.checker.IsMountPoint(mountPath)
	if !mounted {
		return
	}

	refs, err := m.getMountRefs(mountPath)
	if err != nil {
		klog.Warningf("Late adopt: cannot get mount refs for %s: %v", mountPath, err)
		return
	}

	m.mu.Lock()
	for _, ref := range refs {
		if ref != mountPath {
			if _, exists := m.binds[ref]; !exists {
				m.binds[ref] = mountPath
				klog.Infof("Late adopt: restored bind %s -> %s", ref, mountPath)
			}
		}
	}
	m.mu.Unlock()
}

// cleanupSource handles cleanup when a mount pod enters a terminal phase or
// when periodic cleanup finds an orphaned source mount. It unmounts the FUSE
// source and all tracked bind mounts if no kernel references remain.
func (m *PodMounter) cleanupSource(mountPath string) {
	lk := m.acquireSourceLock(mountPath)
	lk.Lock()
	defer func() {
		lk.Unlock()
		m.releaseSourceLock(mountPath, lk)
	}()

	mountRefs, refErr := m.getMountRefs(mountPath)
	if refErr != nil {
		klog.Warningf("Cannot enumerate mount refs for %s, deferring cleanup: %v", mountPath, refErr)
		return
	}
	if len(mountRefs) > 0 {
		klog.V(4).Infof("Source %s still has %d kernel refs, deferring cleanup", mountPath, len(mountRefs))
		return
	}

	klog.Infof("Cleaning up source %s (no kernel refs)", mountPath)
	_ = fuseUnmount(mountPath)

	m.mu.Lock()
	var staleTargets []string
	for target, source := range m.binds {
		if source == mountPath {
			staleTargets = append(staleTargets, target)
		}
	}
	for _, target := range staleTargets {
		delete(m.binds, target)
	}
	m.mu.Unlock()

	for _, target := range staleTargets {
		_ = fuseUnmount(target)
	}
}

// rebindTargets checks all targets bound to the given source mount path.
// If the FUSE mount is alive but a bind mount is stale, re-create it.
func (m *PodMounter) rebindTargets(mountPath string) {
	lk := m.acquireSourceLock(mountPath)
	lk.Lock()
	defer func() {
		lk.Unlock()
		m.releaseSourceLock(mountPath, lk)
	}()

	// Wait for FUSE mount to appear after pod restart.
	for i := 0; i < 30; i++ {
		mounted, err := m.checker.IsMountPoint(mountPath)
		if err == nil && mounted {
			break
		}
		time.Sleep(time.Second)
	}

	mounted, err := m.checker.IsMountPoint(mountPath)
	if err != nil || !mounted {
		klog.Warningf("FUSE mount not available at %s after pod restart", mountPath)
		return
	}

	m.mu.Lock()
	var targets []string
	for target, source := range m.binds {
		if source == mountPath {
			targets = append(targets, target)
		}
	}
	m.mu.Unlock()

	for _, target := range targets {
		// Re-check under lock: if Unmount removed this entry, skip it.
		m.mu.Lock()
		currentSource, stillTracked := m.binds[target]
		m.mu.Unlock()
		if !stillTracked || currentSource != mountPath {
			continue
		}

		// Check if the bind mount is still valid: must be a mount point AND not stale.
		targetMounted, mountErr := m.checker.IsMountPoint(target)
		if mountErr == nil && targetMounted && !isMountStale(target) {
			klog.V(4).Infof("Bind mount at %s still valid", target)
			continue
		}

		klog.Infof("Re-binding %s -> %s after pod restart", mountPath, target)
		_ = fuseUnmount(target)
		if err := os.MkdirAll(target, 0750); err != nil {
			klog.Warningf("Failed to create target directory %s: %v", target, err)
			continue
		}
		if err := bindMount(mountPath, target); err != nil {
			klog.Warningf("Failed to re-bind %s -> %s: %v", mountPath, target, err)
		} else {
			klog.Infof("Successfully re-bound %s -> %s", mountPath, target)
		}
	}
}

func (m *PodMounter) Mount(sourceType, sourceID, target string, opts MountOptions) error {
	volumeID := mountID(target)
	mountPath := filepath.Join(mountBaseDir, volumeID)
	podName := mountPodPrefix + volumeID

	containerMountPath := fmt.Sprintf("/mnt/hf/%s", volumeID)
	args, err := buildArgs(sourceType, sourceID, containerMountPath, opts)
	if err != nil {
		return err
	}

	ctx := context.TODO()

	lk := m.acquireSourceLock(mountPath)
	lk.Lock()
	defer func() {
		lk.Unlock()
		m.releaseSourceLock(mountPath, lk)
	}()

	// Create HFMount CRD (best-effort, source of truth for kubectl visibility).
	crdName := hfMountName(volumeID)
	logCRDError("create", crdName, m.crd.create(ctx, crdName, m.nodeID, sourceType, sourceID, podName, mountPath))

	createdPod := false
	pod := m.buildMountPod(podName, volumeID, sourceType, sourceID, mountPath, args)
	klog.Infof("Creating mount pod %s for %s %s", podName, sourceType, sourceID)
	if _, err := m.client.CoreV1().Pods(m.namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		if errors.IsAlreadyExists(err) {
			klog.V(4).Infof("Mount pod %s already exists, reusing", podName)
		} else {
			return fmt.Errorf("failed to create mount pod %s: %w", podName, err)
		}
	} else {
		createdPod = true
	}

	cleanup := createdPod
	defer func() {
		if cleanup {
			klog.Warningf("Mount failed, cleaning up pod %s", podName)
			_ = m.client.CoreV1().Pods(m.namespace).Delete(ctx, podName, metav1.DeleteOptions{})
		}
	}()

	if err := m.waitForPodRunning(ctx, podName); err != nil {
		return fmt.Errorf("mount pod %s did not become running: %w", podName, err)
	}

	if err := m.waitForMount(mountPath); err != nil {
		return fmt.Errorf("FUSE mount did not appear at %s: %w", mountPath, err)
	}

	if err := os.MkdirAll(target, 0750); err != nil {
		return fmt.Errorf("failed to create target directory %s: %w", target, err)
	}
	if err := bindMount(mountPath, target); err != nil {
		return fmt.Errorf("bind mount %s -> %s failed: %w", mountPath, target, err)
	}

	m.mu.Lock()
	m.binds[target] = mountPath
	m.mu.Unlock()

	logCRDError("addTarget", crdName, m.crd.addTarget(ctx, crdName, target))
	logCRDError("updateStatus", crdName, m.crd.updateStatus(ctx, crdName, "Mounted", ""))

	cleanup = false
	klog.Infof("Successfully mounted %s %s at %s (via pod %s)", sourceType, sourceID, target, podName)
	return nil
}

func (m *PodMounter) Unmount(target string) error {
	// Derive source early to acquire the source lock, serializing with
	// rebindTargets and handlePodDelete operating on the same source.
	m.mu.Lock()
	source, tracked := m.binds[target]
	m.mu.Unlock()

	if !tracked {
		source = filepath.Join(mountBaseDir, mountID(target))
	}

	lk := m.acquireSourceLock(source)
	lk.Lock()
	defer func() {
		lk.Unlock()
		m.releaseSourceLock(source, lk)
	}()

	if err := fuseUnmount(target); err != nil {
		klog.V(4).Infof("lazy unmount %s: %v", target, err)
	}

	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove mount target %s: %w", target, err)
	}

	m.mu.Lock()
	// Re-read under lock in case state changed while acquiring source lock.
	source, tracked = m.binds[target]
	if tracked {
		delete(m.binds, target)
	}
	sourceInUse := false
	if tracked {
		for _, s := range m.binds {
			if s == source {
				sourceInUse = true
				break
			}
		}
	}
	m.mu.Unlock()

	if !tracked {
		volumeID := mountID(target)
		podName := mountPodPrefix + volumeID
		crdName := hfMountName(volumeID)
		derivedSource := filepath.Join(mountBaseDir, volumeID)

		mounted, _ := m.checker.IsMountPoint(derivedSource)
		if mounted {
			klog.Infof("Untracked target %s: found FUSE mount at %s, cleaning up pod %s", target, derivedSource, podName)
			_ = fuseUnmount(derivedSource)
			_ = m.client.CoreV1().Pods(m.namespace).Delete(context.TODO(), podName, metav1.DeleteOptions{})
			logCRDError("delete", crdName, m.crd.delete(context.TODO(), crdName))
		}
		return nil
	}

	volumeID := mountIDFromSource(source)
	crdName := hfMountName(volumeID)

	if sourceInUse {
		klog.V(4).Infof("Source %s still in use by other targets, keeping mount pod", source)
		logCRDError("removeTarget", crdName, func() error {
			_, err := m.crd.removeTarget(context.TODO(), crdName, target)
			return err
		}())
		return nil
	}

	podName := mountPodPrefix + volumeID
	klog.Infof("Deleting mount pod %s (no more references)", podName)
	if err := m.client.CoreV1().Pods(m.namespace).Delete(context.TODO(), podName, metav1.DeleteOptions{}); err != nil {
		if !errors.IsNotFound(err) {
			klog.Warningf("Failed to delete mount pod %s: %v", podName, err)
		}
	}
	logCRDError("delete", crdName, m.crd.delete(context.TODO(), crdName))

	return nil
}

func (m *PodMounter) IsMountPoint(target string) (bool, error) {
	return m.checker.IsMountPoint(target)
}

// Recover re-adopts existing mount pods and rebuilds the binds map
// by scanning /proc/self/mountinfo for bind mounts of each FUSE source.
func (m *PodMounter) Recover() error {
	ctx := context.TODO()

	pods, err := m.client.CoreV1().Pods(m.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s", labelApp, labelAppValue, labelNode, sanitizeLabelValue(m.nodeID)),
	})
	if err != nil {
		return fmt.Errorf("failed to list mount pods: %w", err)
	}

	klog.Infof("Recovery: found %d mount pods on node %s", len(pods.Items), m.nodeID)

	for _, pod := range pods.Items {
		mountPath := pod.Annotations[annotMountPath]
		if mountPath == "" {
			klog.Warningf("Recovery: mount pod %s has no mount-path annotation, skipping", pod.Name)
			continue
		}

		mounted, err := m.checker.IsMountPoint(mountPath)
		if err != nil {
			klog.Warningf("Recovery: mount check failed for %s (pod %s): %v", mountPath, pod.Name, err)
			if mount.IsCorruptedMnt(err) {
				klog.Warningf("Recovery: stale mount at %s, cleaning up pod %s", mountPath, pod.Name)
				_ = fuseUnmount(mountPath)
				_ = m.client.CoreV1().Pods(m.namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
			}
			continue
		}

		if !mounted {
			if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
				klog.Warningf("Recovery: pod %s is %s with no mount, deleting", pod.Name, pod.Status.Phase)
				_ = m.client.CoreV1().Pods(m.namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
				continue
			}
			// Pod is running but mount not ready yet. Wait up to 30s for it.
			klog.Infof("Recovery: pod %s running, waiting for mount at %s", pod.Name, mountPath)
			for i := 0; i < 30; i++ {
				time.Sleep(time.Second)
				mounted, err = m.checker.IsMountPoint(mountPath)
				if err == nil && mounted {
					break
				}
			}
			if !mounted {
				klog.Warningf("Recovery: mount never appeared at %s for pod %s, skipping", mountPath, pod.Name)
				continue
			}
		}

		// Rebuild binds: find all mount references to this FUSE source.
		// Retry up to 3 times on failure to avoid partial recovery.
		var refs []string
		var refErr error
		for attempt := 0; attempt < 3; attempt++ {
			refs, refErr = m.getMountRefs(mountPath)
			if refErr == nil {
				break
			}
			klog.Warningf("Recovery: failed to get mount refs for %s (attempt %d/3): %v", mountPath, attempt+1, refErr)
			time.Sleep(time.Second)
		}
		if refErr != nil {
			klog.Errorf("Recovery: cannot enumerate mount refs for %s after retries, skipping pod %s (binds may be incomplete)", mountPath, pod.Name)
			continue
		}
		m.mu.Lock()
		for _, ref := range refs {
			if ref != mountPath {
				m.binds[ref] = mountPath
				klog.V(4).Infof("Recovery: restored bind %s -> %s", ref, mountPath)
			}
		}
		m.mu.Unlock()

		klog.Infof("Recovery: re-adopted mount pod %s with mount at %s (%d bind refs)", pod.Name, mountPath, len(refs))
	}

	return nil
}

func (m *PodMounter) buildMountPod(name, volumeID, sourceType, sourceID, mountPath string, args []string) *corev1.Pod {
	bidirectional := corev1.MountPropagationBidirectional

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: m.namespace,
			Labels: map[string]string{
				labelApp:      labelAppValue,
				labelVolumeID: sanitizeLabelValue(volumeID),
				labelNode:     sanitizeLabelValue(m.nodeID),
			},
			Annotations: map[string]string{
				annotSourceType: sourceType,
				annotSourceID:   sourceID,
				annotMountPath:  mountPath,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyOnFailure,
			TerminationGracePeriodSeconds: ptr.To(int64(30)),
			Affinity: &corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{{
							MatchFields: []corev1.NodeSelectorRequirement{{
								Key:      "metadata.name",
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{m.nodeID},
							}},
						}},
					},
				},
			},
			ImagePullSecrets: m.imagePullSecrets,
			Tolerations: []corev1.Toleration{{
				Operator: corev1.TolerationOpExists,
			}},
			Containers: []corev1.Container{{
				Name:            "hf-mount",
				Image:           m.image,
				ImagePullPolicy: m.imagePullPolicy,
				Command:         []string{hfMountBinary},
				Args:            args,
				SecurityContext: &corev1.SecurityContext{
					Privileged: ptr.To(true),
				},
				VolumeMounts: []corev1.VolumeMount{
					{
						Name:             "mnt-dir",
						MountPath:        "/mnt/hf",
						MountPropagation: &bidirectional,
					},
					{
						Name:      "cache-dir",
						MountPath: m.cacheDir,
					},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10m"),
						corev1.ResourceMemory: resource.MustParse("32Mi"),
					},
				},
			}},
			Volumes: []corev1.Volume{
				{
					Name: "mnt-dir",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: mountBaseDir,
							Type: ptr.To(corev1.HostPathDirectoryOrCreate),
						},
					},
				},
				{
					Name: "cache-dir",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: m.cacheDir,
							Type: ptr.To(corev1.HostPathDirectoryOrCreate),
						},
					},
				},
			},
		},
	}
}

func (m *PodMounter) waitForPodRunning(ctx context.Context, name string) error {
	deadline := time.After(podReadyTimeout)
	ticker := time.NewTicker(podReadyPoll)
	defer ticker.Stop()

	var lastErr error
	for {
		select {
		case <-deadline:
			if lastErr != nil {
				return fmt.Errorf("timeout waiting for pod %s to be running: %w", name, lastErr)
			}
			return fmt.Errorf("timeout waiting for pod %s to be running", name)
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			pod, err := m.client.CoreV1().Pods(m.namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				lastErr = err
				continue
			}
			switch pod.Status.Phase {
			case corev1.PodRunning:
				return nil
			case corev1.PodFailed, corev1.PodSucceeded:
				return fmt.Errorf("pod %s is in terminal phase %s", name, pod.Status.Phase)
			}
		}
	}
}

func (m *PodMounter) waitForMount(path string) error {
	deadline := time.After(mountTimeoutPM)
	ticker := time.NewTicker(mountReadyPollPM)
	defer ticker.Stop()

	var lastErr error
	for {
		select {
		case <-deadline:
			if lastErr != nil {
				return fmt.Errorf("timeout waiting for mount at %s: %w", path, lastErr)
			}
			return fmt.Errorf("timeout waiting for mount at %s", path)
		case <-ticker.C:
			mounted, err := m.checker.IsMountPoint(path)
			if err != nil {
				lastErr = err
				continue
			}
			if mounted {
				return nil
			}
		}
	}
}

// totalRestartCount returns the sum of restartCount across all container statuses.
func totalRestartCount(pod *corev1.Pod) int32 {
	var total int32
	for _, cs := range pod.Status.ContainerStatuses {
		total += cs.RestartCount
	}
	return total
}

func mountID(target string) string {
	h := sha256.Sum256([]byte(target))
	return fmt.Sprintf("%x", h[:6])
}

func mountIDFromSource(source string) string {
	return filepath.Base(source)
}

func sanitizeLabelValue(v string) string {
	v = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, v)
	if len(v) > 63 {
		v = v[:63]
	}
	return v
}
