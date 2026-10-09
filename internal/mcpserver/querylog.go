package mcpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/haileyok/engram-garden/internal/agent"
)

// QueryLog appends each recall's query and results to a file on the
// agent's own machine, one JSON object per line, for evaluating search
// (engram-eval gen -log). It's opt-in: engram-mcp writes one only when
// ENGRAM_QUERY_LOG names a file. The appview never logs query text.
type QueryLog struct {
	mu   sync.Mutex
	path string
	log  *slog.Logger
}

// NewQueryLog logs to path, creating it readable only by its owner.
func NewQueryLog(path string, log *slog.Logger) *QueryLog {
	if log == nil {
		log = slog.Default()
	}
	return &QueryLog{path: path, log: log}
}

// QueryLogEntry is one logged recall.
type QueryLogEntry struct {
	Time    time.Time `json:"time"`
	Space   string    `json:"space,omitempty"`
	Query   string    `json:"query"`
	Results []string  `json:"results"`
}

// Record appends one recall. A failure is logged, never returned: logging
// must not break recall.
func (l *QueryLog) Record(in agent.RecallIn, out agent.MemoriesOut) {
	e := QueryLogEntry{Time: time.Now().UTC(), Space: in.Space, Query: in.Query, Results: []string{}}
	for _, m := range out.Memories {
		e.Results = append(e.Results, m.URI)
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		l.log.Warn("query log", "err", err)
		return
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		l.log.Warn("query log", "err", err)
	}
	if err := f.Close(); err != nil {
		l.log.Warn("query log", "err", err)
	}
}

// logged wraps recall so successful calls are recorded.
func (l *QueryLog) logged(recall func(context.Context, agent.RecallIn) (agent.MemoriesOut, error)) func(context.Context, agent.RecallIn) (agent.MemoriesOut, error) {
	if l == nil {
		return recall
	}
	return func(ctx context.Context, in agent.RecallIn) (agent.MemoriesOut, error) {
		out, err := recall(ctx, in)
		if err == nil {
			l.Record(in, out)
		}
		return out, err
	}
}

// Option configures NewServer.
type Option func(*options)

type options struct {
	queryLog *QueryLog
}

// WithQueryLog records every recall in l.
func WithQueryLog(l *QueryLog) Option {
	return func(o *options) { o.queryLog = l }
}
