package plasmactlbump

import (
	"testing"
)

func TestComposeVersion(t *testing.T) {
	tests := []struct {
		name       string
		oldVersion string
		newVersion string
		want       string
	}{
		{
			name:       "old without dash: appends new",
			oldVersion: "ver1",
			newVersion: "abc1234567890",
			want:       "ver1-abc1234567890",
		},
		{
			name:       "old with dash: replaces propagated part",
			oldVersion: "ver1-old1234567890",
			newVersion: "new1234567890",
			want:       "ver1-new1234567890",
		},
		{
			name:       "new already contains dash: returned as is",
			oldVersion: "ver1",
			newVersion: "ver2-abc1234567890",
			want:       "ver2-abc1234567890",
		},
		{
			name:       "new with dash overrides old with dash",
			oldVersion: "ver1-old1234567890",
			newVersion: "ver2-new1234567890",
			want:       "ver2-new1234567890",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := composeVersion(tt.oldVersion, tt.newVersion)
			if got != tt.want {
				t.Errorf("composeVersion(%q, %q) = %q, want %q", tt.oldVersion, tt.newVersion, got, tt.want)
			}
		})
	}
}
