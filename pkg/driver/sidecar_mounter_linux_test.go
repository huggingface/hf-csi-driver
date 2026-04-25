//go:build linux

package driver

import "testing"

func TestSidecarFuseFdCount(t *testing.T) {
	tests := []struct {
		name      string
		envVar    string // value of HF_CSI_SIDECAR_FUSE_FD_COUNT (empty = unset)
		volumeArg string // value of `fuseFdCount` volumeAttribute
		want      int
	}{
		{name: "default", envVar: "", volumeArg: "", want: defaultSidecarFuseFdCount},

		{name: "env override", envVar: "8", volumeArg: "", want: 8},
		{name: "env=1 (legacy compat)", envVar: "1", volumeArg: "", want: 1},
		{name: "env invalid falls back to default", envVar: "abc", volumeArg: "", want: defaultSidecarFuseFdCount},
		{name: "env=0 falls back to default", envVar: "0", volumeArg: "", want: defaultSidecarFuseFdCount},

		{name: "volume override beats default", envVar: "", volumeArg: "6", want: 6},
		{name: "volume override beats env", envVar: "8", volumeArg: "2", want: 2},
		{name: "volume invalid keeps env", envVar: "8", volumeArg: "abc", want: 8},

		{name: "clamp to 32", envVar: "100", volumeArg: "", want: 32},
		{name: "volume clamp to 32", envVar: "", volumeArg: "9999", want: 32},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HF_CSI_SIDECAR_FUSE_FD_COUNT", tc.envVar)
			got := sidecarFuseFdCount(tc.volumeArg)
			if got != tc.want {
				t.Errorf("sidecarFuseFdCount(env=%q, vol=%q) = %d, want %d", tc.envVar, tc.volumeArg, got, tc.want)
			}
		})
	}
}
