package protocol

import "testing"

func TestBuildClientVersion(t *testing.T) {
	tests := []struct {
		version string
		want    uint32
	}{
		{"0.1.0", 0x00000100},
		{"1.0.0", 0x00010000},
		{"1.2.3", 0x00010203},
		{"0.2.1", 0x00000201},
		{"v0.2.1", 0x00000201},
		{"1.2.3-rc.1", 0x00010203},
		{"1.2.3+build.5", 0x00010203},
	}

	for _, tc := range tests {
		got := buildClientVersion(tc.version)
		if got != tc.want {
			t.Errorf("buildClientVersion(%q) = 0x%08x, want 0x%08x", tc.version, got, tc.want)
		}
	}
}
