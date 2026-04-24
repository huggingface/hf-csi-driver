package driver

import "testing"

func TestPodUIDFromTarget(t *testing.T) {
	cases := map[string]string{
		"/var/lib/kubelet/pods/abc-def/volumes/kubernetes.io~csi/foo/mount": "abc-def",
		"/var/lib/kubelet/pods/29e21681-8546-4e1b-b308-136d9c1bb9e3/volumes/kubernetes.io~csi/doc-bucket/mount": "29e21681-8546-4e1b-b308-136d9c1bb9e3",
		"/other/path":                  "",
		"/var/lib/kubelet/pods/":       "",
		"/var/lib/kubelet/pods/abc":    "",
		"/var/lib/kubelet/pods//x":     "",
	}
	for in, want := range cases {
		if got := podUIDFromTarget(in); got != want {
			t.Errorf("podUIDFromTarget(%q) = %q, want %q", in, got, want)
		}
	}
}
