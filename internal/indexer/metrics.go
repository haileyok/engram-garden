package indexer

import (
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/haileyok/engram-garden/internal/metrics"
)

var (
	repoSyncs = metrics.Factory.NewCounterVec(prometheus.CounterOpts{
		Name: "engram_repo_syncs_total",
		Help: "Repo syncs, by how (incremental from the oplog, or full export) and result.",
	}, []string{"mode", "result"})
	repoSyncDuration = metrics.Factory.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "engram_repo_sync_duration_seconds",
		Help:    "Time to sync one repo, by how.",
		Buckets: metrics.DurationBuckets,
	}, []string{"mode"})
	reexports = metrics.Factory.NewCounter(prometheus.CounterOpts{
		Name: "engram_repo_reexports_total",
		Help: "Incremental syncs that couldn't be applied or verified, so the repo was exported in full instead.",
	})
	recordsIndexed = metrics.Factory.NewCounterVec(prometheus.CounterOpts{
		Name: "engram_records_indexed_total",
		Help: "Memory records applied to the index: upserts and deletes.",
	}, []string{"op"})
	indexLag = metrics.Factory.NewHistogram(prometheus.HistogramOpts{
		Name:    "engram_index_lag_seconds",
		Help:    "Time from a repo commit to its changes being in the index, for incremental syncs: how long a new memory takes to become searchable.",
		Buckets: []float64{.25, .5, 1, 2, 5, 10, 30, 60, 120, 300, 600, 1800, 3600},
	})
	overLimit = metrics.Factory.NewCounter(prometheus.CounterOpts{
		Name: "engram_index_over_limit_total",
		Help: "Repo changes the index refused, wholly or partly, because the space is over a limit.",
	})
	spaceSyncs = metrics.Factory.NewCounterVec(prometheus.CounterOpts{
		Name: "engram_space_syncs_total",
		Help: "Whole-space syncs (list the writers, sync each repo that changed), by result.",
	}, []string{"result"})
	spaceSyncDuration = metrics.Factory.NewHistogram(prometheus.HistogramOpts{
		Name:    "engram_space_sync_duration_seconds",
		Help:    "Time for a whole-space sync.",
		Buckets: metrics.SlowBuckets,
	})
)

// observeLag records how long ago a commit was made, from its rev (a TID).
func observeLag(rev string, now time.Time) {
	tid, err := syntax.ParseTID(rev)
	if err != nil {
		return
	}
	if lag := now.Sub(tid.Time()); lag >= 0 {
		indexLag.Observe(lag.Seconds())
	}
}
