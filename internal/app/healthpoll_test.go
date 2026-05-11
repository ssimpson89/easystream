package app

import "testing"

func TestDestinationRestartReason(t *testing.T) {
	tests := []struct {
		name string
		snap streamHealthSnapshot
		want bool
	}{
		{
			name: "good",
			snap: streamHealthSnapshot{StreamStatus: "active", HealthStatus: "good"},
			want: false,
		},
		{
			name: "ok",
			snap: streamHealthSnapshot{StreamStatus: "active", HealthStatus: "ok"},
			want: false,
		},
		{
			name: "youtube no data",
			snap: streamHealthSnapshot{StreamStatus: "active", HealthStatus: "noData"},
			want: true,
		},
		{
			name: "inactive",
			snap: streamHealthSnapshot{StreamStatus: "inactive", HealthStatus: "noData"},
			want: true,
		},
		{
			name: "bad quality is not restart worthy",
			snap: streamHealthSnapshot{StreamStatus: "active", HealthStatus: "bad"},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := destinationRestartReason(tt.snap)
			if got != tt.want {
				t.Fatalf("destinationRestartReason() = (%v, %q), want restart=%v", got, reason, tt.want)
			}
			if got && reason == "" {
				t.Fatal("restart reason should not be empty")
			}
		})
	}
}
