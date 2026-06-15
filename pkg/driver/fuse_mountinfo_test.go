package driver

import (
	"strings"
	"testing"
)

func TestParseFuseMinor(t *testing.T) {
	const mountinfo = `22 28 0:21 / /sys rw,nosuid - sysfs sysfs rw
600 30 0:142 / /var/lib/kubelet/pods/abc/volumes/kubernetes.io~csi/hf-vol-0/mount rw,nosuid,nodev,relatime shared:300 - fuse.hf-mount hf-mount rw,user_id=0,group_id=0
601 30 0:143 / /var/lib/kubelet/pods/def/volumes/kubernetes.io~csi/hf-vol-0/mount rw - ext4 /dev/x rw
`
	t.Run("matches fuse mount and extracts minor", func(t *testing.T) {
		minor, ok := parseFuseMinor(strings.NewReader(mountinfo),
			"/var/lib/kubelet/pods/abc/volumes/kubernetes.io~csi/hf-vol-0/mount")
		if !ok || minor != 142 {
			t.Fatalf("got (minor=%d, ok=%v), want (142, true)", minor, ok)
		}
	})

	t.Run("ignores non-fuse mount at same-shaped path", func(t *testing.T) {
		if _, ok := parseFuseMinor(strings.NewReader(mountinfo),
			"/var/lib/kubelet/pods/def/volumes/kubernetes.io~csi/hf-vol-0/mount"); ok {
			t.Fatal("expected no match for ext4 mount")
		}
	})

	t.Run("no match for unknown path", func(t *testing.T) {
		if _, ok := parseFuseMinor(strings.NewReader(mountinfo), "/not/mounted"); ok {
			t.Fatal("expected no match")
		}
	})
}
