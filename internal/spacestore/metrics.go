package spacestore

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/haileyok/engram-garden/internal/metrics"
)

var (
	searches = metrics.Factory.NewCounterVec(prometheus.CounterOpts{
		Name: "engram_searches_total",
		Help: "Searches, by result: ok, rate_limited, retry (the space is still loading or busy), canceled, error.",
	}, []string{"result"})
	searchDuration = metrics.Factory.NewHistogram(prometheus.HistogramOpts{
		Name:    "engram_search_duration_seconds",
		Help:    "Time to run a search, loading the space included.",
		Buckets: metrics.DurationBuckets,
	})
	flushes = metrics.Factory.NewCounterVec(prometheus.CounterOpts{
		Name: "engram_flushes_total",
		Help: "Write buffer flushes (new segment and manifest), by result.",
	}, []string{"result"})
	flushDuration = metrics.Factory.NewHistogram(prometheus.HistogramOpts{
		Name:    "engram_flush_duration_seconds",
		Help:    "Time to flush a space's write buffer.",
		Buckets: metrics.DurationBuckets,
	})
	maintenance = metrics.Factory.NewCounterVec(prometheus.CounterOpts{
		Name: "engram_maintenance_total",
		Help: "Segment merges and garbage collections, by kind (merge, gc) and result.",
	}, []string{"kind", "result"})
	maintenanceDuration = metrics.Factory.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "engram_maintenance_duration_seconds",
		Help:    "Time for a merge or garbage collection, by kind.",
		Buckets: metrics.SlowBuckets,
	}, []string{"kind"})
	loads = metrics.Factory.NewCounterVec(prometheus.CounterOpts{
		Name: "engram_space_loads_total",
		Help: "Spaces loaded into memory, by result.",
	}, []string{"result"})
	loadDuration = metrics.Factory.NewHistogram(prometheus.HistogramOpts{
		Name:    "engram_space_load_duration_seconds",
		Help:    "Time to load a space's index.",
		Buckets: metrics.SlowBuckets,
	})
	evictions = metrics.Factory.NewCounter(prometheus.CounterOpts{
		Name: "engram_space_evictions_total",
		Help: "Clean spaces dropped from memory to stay within the RAM budget.",
	})
	segmentReads = metrics.Factory.NewCounterVec(prometheus.CounterOpts{
		Name: "engram_segment_reads_total",
		Help: "Range reads of segment files, by where they were served from (disk_cache, object_storage).",
	}, []string{"source"})
	segmentReadBytes = metrics.Factory.NewCounterVec(prometheus.CounterOpts{
		Name: "engram_segment_read_bytes_total",
		Help: "Bytes read from segment files, by where they were served from.",
	}, []string{"source"})
)

func searchResult(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, ErrRetryable):
		return "retry"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return "error"
}

func observeMaintenance(kind string, start time.Time, err error) {
	maintenance.WithLabelValues(kind, metrics.Result(err)).Inc()
	maintenanceDuration.WithLabelValues(kind).Observe(time.Since(start).Seconds())
}

// The collector reports the state of every open node: what's loaded, the
// RAM and disk cache in use. Nodes join when created and leave when closed.
var liveNodes sync.Map // *Node -> struct{}

func init() { metrics.Registry.MustRegister(nodeCollector{}) }

type nodeCollector struct{}

var (
	descSpacesLoaded  = prometheus.NewDesc("engram_spaces_loaded", "Spaces loaded in memory.", nil, nil)
	descRAM           = prometheus.NewDesc("engram_index_ram_bytes", "RAM used by loaded spaces' indexes and 1-bit sections.", nil, nil)
	descRAMBudget     = prometheus.NewDesc("engram_index_ram_budget_bytes", "RAM budget for loaded spaces (ENGRAM_RAM_BYTES).", nil, nil)
	descMemories      = prometheus.NewDesc("engram_memories", "Memories in the loaded spaces' indexes.", nil, nil)
	descBuffered      = prometheus.NewDesc("engram_buffered_memories", "Changes waiting in loaded spaces' write buffers.", nil, nil)
	descSegments      = prometheus.NewDesc("engram_segments", "Segment files of the loaded spaces.", nil, nil)
	descCache         = prometheus.NewDesc("engram_disk_cache_bytes", "Bytes of segment files in the local disk cache.", nil, nil)
	descCacheBudget   = prometheus.NewDesc("engram_disk_cache_budget_bytes", "Disk cache budget (ENGRAM_CACHE_BYTES).", nil, nil)
	descCacheFiles    = prometheus.NewDesc("engram_disk_cache_files", "Segment files in the local disk cache.", nil, nil)
	descSpaceMemories = prometheus.NewDesc("engram_space_memories", "Memories in a loaded space's index (per-space metrics only).", []string{"space"}, nil)
	descSpaceBuffered = prometheus.NewDesc("engram_space_buffered_memories", "Changes in a loaded space's write buffer (per-space metrics only).", []string{"space"}, nil)
	descSpaceSegments = prometheus.NewDesc("engram_space_segments", "A loaded space's segment files (per-space metrics only).", []string{"space"}, nil)
	descSpaceRAM      = prometheus.NewDesc("engram_space_ram_bytes", "RAM used by a loaded space (per-space metrics only).", []string{"space"}, nil)
)

func (nodeCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descSpacesLoaded, descRAM, descRAMBudget, descMemories, descBuffered, descSegments,
		descCache, descCacheBudget, descCacheFiles, descSpaceMemories, descSpaceBuffered, descSpaceSegments, descSpaceRAM} {
		ch <- d
	}
}

func (nodeCollector) Collect(ch chan<- prometheus.Metric) {
	var loaded, ram, budget, mems, buffered, segs, cache, cacheBudget, cacheFiles float64
	found := false
	liveNodes.Range(func(k, _ any) bool {
		found = true
		n := k.(*Node)
		budget += float64(n.opt.RAMBytes)
		n.mu.Lock()
		spaces := make([]*Space, 0, len(n.spaces))
		for _, s := range n.spaces {
			spaces = append(spaces, s)
		}
		n.mu.Unlock()
		for _, s := range spaces {
			s.mu.RLock()
			m, b, sg, rb := float64(len(s.locs)), float64(len(s.buf)), 0.0, float64(s.ramBytes)
			for _, sl := range s.slots {
				if sl != nil {
					sg += float64(len(sl.segs))
				}
			}
			s.mu.RUnlock()
			loaded++
			ram, mems, buffered, segs = ram+rb, mems+m, buffered+b, segs+sg
			if n.opt.PerSpaceMetrics {
				ch <- prometheus.MustNewConstMetric(descSpaceMemories, prometheus.GaugeValue, m, s.uri)
				ch <- prometheus.MustNewConstMetric(descSpaceBuffered, prometheus.GaugeValue, b, s.uri)
				ch <- prometheus.MustNewConstMetric(descSpaceSegments, prometheus.GaugeValue, sg, s.uri)
				ch <- prometheus.MustNewConstMetric(descSpaceRAM, prometheus.GaugeValue, rb, s.uri)
			}
		}
		if c := n.cache; c != nil {
			c.mu.Lock()
			cache += float64(c.used)
			cacheFiles += float64(c.lru.Len())
			cacheBudget += float64(c.budget)
			c.mu.Unlock()
		}
		return true
	})
	if !found {
		return
	}
	for _, m := range []struct {
		d *prometheus.Desc
		v float64
	}{{descSpacesLoaded, loaded}, {descRAM, ram}, {descRAMBudget, budget}, {descMemories, mems}, {descBuffered, buffered},
		{descSegments, segs}, {descCache, cache}, {descCacheBudget, cacheBudget}, {descCacheFiles, cacheFiles}} {
		ch <- prometheus.MustNewConstMetric(m.d, prometheus.GaugeValue, m.v)
	}
}
