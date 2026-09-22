package driver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func newProbeServer(healthy, corrupted bool) *NodeServer {
	return &NodeServer{
		isHealthyFn:   func(string) bool { return healthy },
		isCorruptedFn: func(string) bool { return corrupted },
	}
}

func TestProbeStagingRecordsOutcomes(t *testing.T) {
	cases := []struct {
		name       string
		healthy    bool
		corrupted  bool
		wantResult string
		wantVerdic bool
		wantUp     float64
	}{
		{"healthy", true, false, probeHealthy, true, 1},
		{"corrupted", false, true, probeCorrupted, false, 0},
		// Unhealthy but not provably dead: the session is still believed up,
		// which is what stops recovery from tearing it down.
		{"unproven", false, false, probeUnmounted, false, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			volumeID := "vol-" + tc.name
			defer forgetVolumeMetrics(volumeID)

			ns := newProbeServer(tc.healthy, tc.corrupted)
			if got := ns.probeStaging(volumeID, "/staging"); got != tc.wantVerdic {
				t.Errorf("probeStaging verdict = %v, want %v", got, tc.wantVerdic)
			}

			counter := mountHealthCheckTotal.WithLabelValues(volumeID, tc.wantResult)
			if got := testutil.ToFloat64(counter); got != 1 {
				t.Errorf("%s counter = %v, want 1", tc.wantResult, got)
			}
			if got := testutil.ToFloat64(mountSessionUp.WithLabelValues(volumeID)); got != tc.wantUp {
				t.Errorf("session_up = %v, want %v", got, tc.wantUp)
			}
		})
	}
}

// seriesForVolume counts the series currently exposed for one volume across
// every metric. Other tests in this package leave series behind on the shared
// registry, so a global count would be meaningless here.
func seriesForVolume(t *testing.T, volumeID string) int {
	t.Helper()

	families, err := metricsRegistry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	count := 0
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "volume" && label.GetValue() == volumeID {
					count++
				}
			}
		}
	}
	return count
}

func TestForgetVolumeMetricsDropsSeries(t *testing.T) {
	ns := newProbeServer(true, false)
	ns.probeStaging("vol-gone", "/staging")
	recordRecovery("vol-gone", recoveryOutcomeSuccess)

	if got := seriesForVolume(t, "vol-gone"); got != 3 {
		t.Fatalf("expected 3 series for the volume before forgetting, got %d", got)
	}

	forgetVolumeMetrics("vol-gone")

	if got := seriesForVolume(t, "vol-gone"); got != 0 {
		t.Errorf("expected the volume's series to be gone, %d remain", got)
	}
}

func TestMetricsHandlerServesDriverSeries(t *testing.T) {
	defer forgetVolumeMetrics("vol-served")
	newProbeServer(true, false).probeStaging("vol-served", "/staging")

	server := httptest.NewServer(metricsHandler())
	defer server.Close()

	resp, err := http.Get(server.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	for _, want := range []string{
		"seaweedfs_csi_mount_health_check_total",
		"seaweedfs_csi_mount_session_up",
		`volume="vol-served"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics output missing %q", want)
		}
	}
}
