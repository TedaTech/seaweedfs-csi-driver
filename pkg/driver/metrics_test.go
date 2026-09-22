package driver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func newProbeServer(healthy, live bool) *NodeServer {
	return &NodeServer{
		isHealthyFn: func(string) bool { return healthy },
		isLiveFn:    func(string) bool { return live },
	}
}

func TestProbeStagingRecordsOutcomes(t *testing.T) {
	cases := []struct {
		name       string
		healthy    bool
		live       bool
		wantResult string
		wantVerdic bool
		wantUp     float64
	}{
		{"healthy", true, true, probeHealthy, true, 1},
		{"dead", false, false, probeDead, false, 0},
		// The probe said no but the mount is still live: the session stays
		// up, which is what stops recovery from tearing it down.
		{"unhealthy", false, true, probeUnhealthy, false, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			volumeID := "metrics-probe-" + tc.name
			defer forgetVolumeMetrics(volumeID)

			ns := newProbeServer(tc.healthy, tc.live)
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
	ns := newProbeServer(true, true)
	ns.probeStaging("metrics-gone", "/staging")
	recordRecovery("metrics-gone", recoveryOutcomeSuccess)

	if got := seriesForVolume(t, "metrics-gone"); got != 3 {
		t.Fatalf("expected 3 series for the volume before forgetting, got %d", got)
	}

	forgetVolumeMetrics("metrics-gone")

	if got := seriesForVolume(t, "metrics-gone"); got != 0 {
		t.Errorf("expected the volume's series to be gone, %d remain", got)
	}
}

func TestMetricsHandlerServesDriverSeries(t *testing.T) {
	defer forgetVolumeMetrics("metrics-served")
	newProbeServer(true, true).probeStaging("metrics-served", "/staging")

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
		`volume="metrics-served"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics output missing %q", want)
		}
	}
}
