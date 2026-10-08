package web

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/haileyok/engram-garden/internal/metrics"
)

var signIns = metrics.Factory.NewCounterVec(prometheus.CounterOpts{
	Name: "engram_web_signins_total",
	Help: "Web app sign-ins finished at the OAuth callback, by result: ok, partial (fewer permissions than requested), declined, failed, refused (not started from this browser).",
}, []string{"result"})

func init() {
	// Start every result at zero, so rates work from the first sign-in and
	// the series show which job is the web app.
	for _, r := range []string{"ok", "partial", "declined", "failed", "refused"} {
		signIns.WithLabelValues(r)
	}
}
