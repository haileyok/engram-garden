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
		Help: "Notifications from space authorities, by kind (write, space_deleted) and result: accepted, deferred (the space was at its sync cap; a running sync picks it up), not_granted (the space's authority hasn't let the appview read it, so the write can't be indexed), bad_body, unauthorized, forbidden, unknown_space, error.",
	}, []string{"kind", "result"})
	notifiedSyncs = metrics.Factory.NewCounterVec(prometheus.CounterOpts{
		Name: "engram_notified_syncs_total",
		Help: "Repo syncs started by a write notification, by result: ok, error, no_slot (gave up waiting for a sync slot).",
	}, []string{"result"})
	notifyRegistrations = metrics.Factory.NewCounterVec(prometheus.CounterOpts{
		Name: "engram_notify_registrations_total",
		Help: "Requests to space authorities to send this service write notifications, by result.",
	}, []string{"result"})
	textSearches = metrics.Factory.NewCounterVec(prometheus.CounterOpts{
		Name: "engram_text_searches_total",
		Help: "Searches that sent query text for the appview to embed, by result: ok, not_allowed (the space's authority isn't on the list), rate_limited (past the authority's limit), model_not_hosted (the appview doesn't run the space's model), busy (too many embeddings at once), error.",
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
		for _, r := range []string{"accepted", "not_granted", "bad_body", "unauthorized", "forbidden", "unknown_space", "error"} {
			notifications.WithLabelValues(kind, r)
		}
	}
	for _, r := range []string{"ok", "error", "no_slot"} {
		notifiedSyncs.WithLabelValues(r)
	}
	for _, r := range []string{"ok", "not_allowed", "rate_limited", "model_not_hosted", "busy", "error"} {
		textSearches.WithLabelValues(r)
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
