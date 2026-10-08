package appview

import (
	"context"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/haileyok/engram-garden/internal/indexer"
	"github.com/haileyok/engram-garden/internal/metrics"
)

// metricValue sums a metric's counters or gauges (or histogram sample
// counts) over the series whose labels include the given ones.
func metricValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
	series:
		for _, m := range mf.GetMetric() {
			have := map[string]string{}
			for _, lp := range m.GetLabel() {
				have[lp.GetName()] = lp.GetValue()
			}
			for k, v := range labels {
				if have[k] != v {
					continue series
				}
			}
			switch mf.GetType() {
			case dto.MetricType_COUNTER:
				total += m.GetCounter().GetValue()
			case dto.MetricType_GAUGE:
				total += m.GetGauge().GetValue()
			case dto.MetricType_HISTOGRAM:
				total += float64(m.GetHistogram().GetSampleCount())
			}
		}
	}
	return total
}

// TestMetricsFollowNotifications: a notified write shows up as an accepted
// notification, a repo sync, indexed records and the time it took to be
// indexed; a malformed notification is counted as rejected.
func TestMetricsFollowNotifications(t *testing.T) {
	t.Parallel()
	f := setup(t)
	if _, err := f.srv.Indexer.Register(context.Background(), f.net.Space, f.srv.ServiceID()); err != nil {
		t.Fatal(err)
	}
	accepted := func() float64 {
		return metricValue(t, "engram_notifications_total", map[string]string{"kind": "write", "result": "accepted"})
	}
	before := map[string]float64{
		"accepted":   accepted(),
		"bad_body":   metricValue(t, "engram_notifications_total", map[string]string{"kind": "write", "result": "bad_body"}),
		"syncs":      metricValue(t, "engram_notified_syncs_total", map[string]string{"result": "ok"}),
		"upserts":    metricValue(t, "engram_records_indexed_total", map[string]string{"op": "upsert"}),
		"lag":        metricValue(t, "engram_index_lag_seconds", nil),
		"searches":   metricValue(t, "engram_searches_total", map[string]string{"result": "ok"}),
		"repo_syncs": metricValue(t, "engram_repo_syncs_total", map[string]string{"result": "ok"}),
	}

	// Two writes, so the second is synced incrementally from the oplog.
	for _, rkey := range []string{"m1", "m2"} {
		f.net.Put(f.alice, indexer.Collection, rkey, memory("remember the milk "+rkey))
		if got := f.net.DeliverWrite(f.alice, ""); len(got) != 1 || got[0] != 200 {
			t.Fatalf("delivery statuses: %v", got)
		}
		f.srv.Jobs.Wait()
	}
	if status, body := f.get(t, f.bob, serviceDID, "garden.engram.searchMemories", searchParams(f.net.Space, "milk")); status != 200 || len(memories(body)) != 2 {
		t.Fatalf("search: %d %v", status, body)
	}
	svc := f.srv.ServiceID()
	if s := f.net.Deliver(svc, "com.atproto.space.notifyWrite", map[string]any{"space": f.net.Space, "repo": f.alice.DID, "hash": "not bytes"},
		f.net.ServiceAuth(svc, "com.atproto.space.notifyWrite")); s != 400 {
		t.Fatalf("malformed notification: %d", s)
	}

	for name, want := range map[string]float64{"accepted": 2, "bad_body": 1, "syncs": 2, "upserts": 2, "lag": 1, "searches": 1, "repo_syncs": 2} {
		var got float64
		switch name {
		case "accepted":
			got = accepted()
		case "bad_body":
			got = metricValue(t, "engram_notifications_total", map[string]string{"kind": "write", "result": "bad_body"})
		case "syncs":
			got = metricValue(t, "engram_notified_syncs_total", map[string]string{"result": "ok"})
		case "upserts":
			got = metricValue(t, "engram_records_indexed_total", map[string]string{"op": "upsert"})
		case "lag":
			got = metricValue(t, "engram_index_lag_seconds", nil)
		case "searches":
			got = metricValue(t, "engram_searches_total", map[string]string{"result": "ok"})
		case "repo_syncs":
			got = metricValue(t, "engram_repo_syncs_total", map[string]string{"result": "ok"})
		}
		// Other tests run in parallel and share the registry: at least.
		if got-before[name] < want {
			t.Errorf("%s: went up by %v, want at least %v", name, got-before[name], want)
		}
	}
	if metricValue(t, "engram_spaces_loaded", nil) < 1 {
		t.Error("no loaded spaces reported")
	}
}
