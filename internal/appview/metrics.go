package appview

import (
	"errors"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/haileyok/engram-garden/internal/metrics"
)

var (
	notifications = metrics.Factory.NewCounterVec(prometheus.CounterOpts{
		Name: "engram_notifications_total",
		Help: "Notifications from space authorities, by kind (write, space_deleted) and result: accepted, deferred (the space was at its sync cap; a running sync picks it up), bad_body, unauthorized, forbidden, unknown_space, error.",
	}, []string{"kind", "result"})
	notifiedSyncs = metrics.Factory.NewCounterVec(prometheus.CounterOpts{
		Name: "engram_notified_syncs_total",
		Help: "Repo syncs started by a write notification, by result: ok, error, no_slot (gave up waiting for a sync slot).",
	}, []string{"result"})
	notifyRegistrations = metrics.Factory.NewCounterVec(prometheus.CounterOpts{
		Name: "engram_notify_registrations_total",
		Help: "Requests to space authorities to send this service write notifications, by result.",
	}, []string{"result"})
	spacesIndexed = metrics.Factory.NewGauge(prometheus.GaugeOpts{
		Name: "engram_spaces_indexed",
		Help: "Spaces the appview indexes: configured, plus those whose authority granted access.",
	})
	spacesOwned = metrics.Factory.NewGauge(prometheus.GaugeOpts{
		Name: "engram_spaces_owned",
		Help: "Indexed spaces this node owns and may read.",
	})
)

func init() {
	// Start the outcomes worth alerting on at zero, so increase() sees the
	// first one.
	for _, kind := range []string{"write", "space_deleted"} {
		for _, r := range []string{"accepted", "bad_body", "unauthorized", "forbidden", "unknown_space", "error"} {
			notifications.WithLabelValues(kind, r)
		}
	}
	for _, r := range []string{"ok", "error", "no_slot"} {
		notifiedSyncs.WithLabelValues(r)
	}
}

// rejection labels why a notification was refused.
func rejection(err error) string {
	var xe *xrpcError
	if !errors.As(err, &xe) {
		return "error"
	}
	switch {
	case xe.name == "UnknownSpace":
		return "unknown_space"
	case xe.status == http.StatusBadRequest:
		return "bad_body"
	case xe.status == http.StatusUnauthorized:
		return "unauthorized"
	case xe.status == http.StatusForbidden:
		return "forbidden"
	}
	return "error"
}
