package driver

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/seaweedfs/seaweedfs/weed/glog"
)

// Health-probe outcomes. The distinction between a slow probe and a dead mount
// is the point of this metric: a probe that did not answer in time says nothing
// about whether the FUSE session is alive, while 'dead' means it is gone or
// corrupted. Treating the first as the second is what tore down live mounts.
const (
	probeHealthy   = "healthy"
	probeTimeout   = "timeout"
	probeUnhealthy = "unhealthy"
	probeDead      = "dead"
)

// Recovery outcomes.
const (
	recoveryOutcomeSuccess          = "success"
	recoveryOutcomeFailed           = "failed"
	recoveryOutcomeSkippedStillLive = "skipped_still_live"
	recoveryOutcomeThrottled        = "throttled"
)

// metricsRegistry is private rather than the global default so the endpoint
// exposes this driver's series only, and not whatever the vendored seaweedfs
// packages happen to register.
var metricsRegistry = prometheus.NewRegistry()

var (
	mountHealthCheckTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "seaweedfs_csi",
		Name:      "mount_health_check_total",
		Help:      "Staging-mount health probes by outcome (healthy, timeout, unhealthy, dead).",
	}, []string{"volume", "result"})

	mountRecoveryTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "seaweedfs_csi",
		Name:      "mount_recovery_total",
		Help:      "Staging-mount recovery attempts by outcome. A recovery stops the weed mount process shared by every consumer pod on the node.",
	}, []string{"volume", "outcome"})

	mountSessionUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "seaweedfs_csi",
		Name:      "mount_session_up",
		Help:      "1 while the volume's FUSE session is a live mount, 0 once it is gone or corrupted.",
	}, []string{"volume"})
)

func init() {
	metricsRegistry.MustRegister(mountHealthCheckTotal, mountRecoveryTotal, mountSessionUp)
}

func recordHealthProbe(volumeID, result string) {
	mountHealthCheckTotal.WithLabelValues(volumeID, result).Inc()
}

func recordRecovery(volumeID, outcome string) {
	mountRecoveryTotal.WithLabelValues(volumeID, outcome).Inc()
}

func setSessionUp(volumeID string, up bool) {
	value := 0.0
	if up {
		value = 1
	}
	mountSessionUp.WithLabelValues(volumeID).Set(value)
}

// forgetVolumeMetrics drops a volume's series once it is unstaged, so a node
// that has seen many short-lived PVCs does not accumulate them forever.
func forgetVolumeMetrics(volumeID string) {
	mountHealthCheckTotal.DeletePartialMatch(prometheus.Labels{"volume": volumeID})
	mountRecoveryTotal.DeletePartialMatch(prometheus.Labels{"volume": volumeID})
	mountSessionUp.DeletePartialMatch(prometheus.Labels{"volume": volumeID})
}

// metricsHandler serves the driver's registry at /metrics.
func metricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(metricsRegistry, promhttp.HandlerOpts{}))
	return mux
}

// ServeMetrics starts the node plugin's Prometheus endpoint. It runs in the
// background and never blocks the caller.
func ServeMetrics(address string) {
	mux := metricsHandler()

	go func() {
		glog.Infof("serving metrics on %s/metrics", address)
		if err := http.ListenAndServe(address, mux); err != nil {
			glog.Errorf("metrics endpoint on %s stopped: %v", address, err)
		}
	}()
}
