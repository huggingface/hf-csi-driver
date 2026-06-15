package driver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mkVolDir creates <root>/pods/<uid>/volumes/kubernetes.io~csi/<vol>/ and
// backdates its mtime past the stale threshold unless stale is false.
func mkVolDir(t *testing.T, root, uid, vol string, stale bool) string {
	t.Helper()
	dir := filepath.Join(root, "pods", uid, "volumes", "kubernetes.io~csi", vol)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if stale {
		old := time.Now().Add(-2 * volDataStaleThreshold)
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func hasVolData(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "vol_data.json"))
	return err == nil
}

func TestReconcileVolData(t *testing.T) {
	root := t.TempDir()

	stuck := mkVolDir(t, root, "pod-stuck", "hf-vol-0", true)     // owned + stale + missing -> repaired
	fresh := mkVolDir(t, root, "pod-fresh", "hf-vol-0", false)    // owned but too new -> skipped
	foreign := mkVolDir(t, root, "pod-foreign", "hf-vol-0", true) // NOT owned -> skipped despite stale+missing+matching name
	healthy := mkVolDir(t, root, "pod-healthy", "hf-vol-0", true) // owned but already has vol_data.json -> untouched
	if err := os.WriteFile(filepath.Join(healthy, "vol_data.json"), []byte(`{"driverName":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Ownership comes from "live pod specs": foreign pod is deliberately absent.
	owned := func() (map[string]map[string]bool, error) {
		return map[string]map[string]bool{
			"pod-stuck":   {"hf-vol-0": true},
			"pod-fresh":   {"hf-vol-0": true},
			"pod-healthy": {"hf-vol-0": true},
		}, nil
	}

	reconcileVolData(filepath.Join(root, "pods"), "node-1", owned)

	if !hasVolData(stuck) {
		t.Error("stuck owned volume should have been repaired")
	}
	if hasVolData(fresh) {
		t.Error("fresh volume must not be touched (avoid racing publish)")
	}
	if hasVolData(foreign) {
		t.Error("unowned volume must not be touched even with a matching name")
	}

	// Repaired content must carry the fields kubelet's NewUnmounter needs.
	buf, err := os.ReadFile(filepath.Join(stuck, "vol_data.json"))
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]string
	if err := json.Unmarshal(buf, &data); err != nil {
		t.Fatal(err)
	}
	if data["driverName"] != DriverName {
		t.Errorf("driverName = %q, want %q", data["driverName"], DriverName)
	}
	if data["specVolID"] != "hf-vol-0" {
		t.Errorf("specVolID = %q, want hf-vol-0", data["specVolID"])
	}
	if data["volumeHandle"] == "" {
		t.Error("volumeHandle must be non-empty for NewUnmounter")
	}

	// Idempotent + race-safe: a second sweep leaves the existing file untouched.
	reconcileVolData(filepath.Join(root, "pods"), "node-1", owned)
	if hb, _ := os.ReadFile(filepath.Join(healthy, "vol_data.json")); string(hb) != `{"driverName":"x"}` {
		t.Error("existing vol_data.json must be left untouched")
	}
}

// A pre-existing vol_data.json (e.g. kubelet won the race) must never be
// overwritten, even if it is a different shape.
func TestRepairVolDataDoesNotClobber(t *testing.T) {
	dir := mkVolDir(t, t.TempDir(), "pod-x", "hf-vol-0", true)
	real := `{"driverName":"hf.csi.huggingface.co","volumeHandle":"csi-real"}`
	if err := os.WriteFile(filepath.Join(dir, "vol_data.json"), []byte(real), 0o600); err != nil {
		t.Fatal(err)
	}
	if repairVolData(dir, "hf-vol-0", "node-1") {
		t.Error("repairVolData must not act when vol_data.json already exists")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "vol_data.json")); string(got) != real {
		t.Errorf("vol_data.json was modified: %s", got)
	}
}
