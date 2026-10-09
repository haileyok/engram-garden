package appview

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/haileyok/cocoon/space"
	"golang.org/x/time/rate"

	"github.com/haileyok/engram-garden/internal/embed"
	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/vec"
)

// MaxTextQueryChars is the longest query text the appview will embed for a
// caller. (A caller that embeds its own query can send up to 4000.)
const MaxTextQueryChars = 1000

// QueryEmbedder embeds the text of search queries for callers that have no
// model of their own, such as the web app and the Claude connector. It runs
// the space's declared model, which the provider checks against the
// space's digest, so a space whose model this service doesn't have gets
// ModelNotHosted rather than a vector that would match nothing.
//
// Anyone who can register a space can reach it, so it's limited: per space
// authority (so one account can't multiply its share with many spaces), and
// by how many embeddings run at once. The appview only embeds after it has
// verified a credential for an indexed space.
type QueryEmbedder struct {
	Provider embed.Provider
	// PerSecond and Burst limit one space authority's text searches
	// (default 2 a second, 20 at once).
	PerSecond float64
	Burst     int
	// MaxConcurrent caps embeddings running at once (default 4). A caller
	// past it is turned away, not queued.
	MaxConcurrent int
	// Timeout bounds one embedding (default 10s).
	Timeout time.Duration
	// Authorities, when set, are the only space authorities that may use
	// it.
	Authorities []string
	// Now tells the time (default time.Now), for tests.
	Now func() time.Time

	mu     sync.Mutex
	bucket map[string]*bucket // by space authority
	swept  time.Time

	semOnce sync.Once
	sem     chan struct{}
}

type bucket struct {
	lim  *rate.Limiter
	seen time.Time
}

// bucketIdle is how long an authority's limiter is kept without searches.
const bucketIdle = 10 * time.Minute

func (q *QueryEmbedder) now() time.Time {
	if q.Now != nil {
		return q.Now()
	}
	return time.Now()
}

// allow takes one of the authority's tokens.
func (q *QueryEmbedder) allow(authority string) bool {
	per, burst := q.PerSecond, q.Burst
	if per <= 0 {
		per = 2
	}
	if burst <= 0 {
		burst = 20
	}
	now := q.now()
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.bucket == nil {
		q.bucket = map[string]*bucket{}
	}
	if now.Sub(q.swept) > time.Minute {
		q.swept = now
		for a, b := range q.bucket {
			if now.Sub(b.seen) > bucketIdle {
				delete(q.bucket, a)
			}
		}
	}
	b := q.bucket[authority]
	if b == nil {
		b = &bucket{lim: rate.NewLimiter(rate.Limit(per), burst)}
		q.bucket[authority] = b
	}
	b.seen = now
	return b.lim.AllowN(now, 1)
}

// tracked is how many authorities have a limiter, for tests.
func (q *QueryEmbedder) tracked() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.bucket)
}

func (q *QueryEmbedder) slot() (func(), bool) {
	q.semOnce.Do(func() {
		n := q.MaxConcurrent
		if n <= 0 {
			n = 4
		}
		q.sem = make(chan struct{}, n)
	})
	select {
	case q.sem <- struct{}{}:
		return func() { <-q.sem }, true
	default:
		return nil, false
	}
}

// Embed returns the query vector for text, using the space's model and
// query prefix, rounded to half precision as a caller's would be. Its
// errors are ones a handler can send.
func (q *QueryEmbedder) Embed(ctx context.Context, spaceURI string, cfg *lex.Config, text string) ([]float32, error) {
	ref, err := space.ParseRef(spaceURI)
	if err != nil {
		return nil, errf(http.StatusBadRequest, "InvalidRequest", "bad space URI")
	}
	if len(q.Authorities) > 0 && !slices.Contains(q.Authorities, ref.Authority) {
		textSearches.WithLabelValues("not_allowed").Inc()
		return nil, errf(http.StatusForbidden, "TextSearchNotAllowed", "this service doesn't embed queries for this space's authority: embed the query with the space's model and send the vector")
	}
	if !q.allow(ref.Authority) {
		textSearches.WithLabelValues("rate_limited").Inc()
		return nil, errf(http.StatusTooManyRequests, "RateLimitExceeded", "too many text searches for this space's authority")
	}
	e, err := q.Provider.For(ctx, cfg.ModelInfo)
	if err != nil {
		var mm *embed.ModelMismatchError
		if errors.As(err, &mm) {
			textSearches.WithLabelValues("model_not_hosted").Inc()
			return nil, errf(http.StatusBadRequest, "ModelNotHosted", "this service doesn't run %s, so it can't embed a query for this space: embed the query with the space's model and send the vector", cfg.ModelInfo)
		}
		textSearches.WithLabelValues("error").Inc()
		return nil, err
	}
	release, ok := q.slot()
	if !ok {
		textSearches.WithLabelValues("busy").Inc()
		return nil, errf(http.StatusServiceUnavailable, "EmbedderBusy", "too many queries are being embedded; try again shortly")
	}
	defer release()
	timeout := q.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	vs, err := e.Embed(ctx, []string{cfg.QueryPrefix + text})
	if err != nil || len(vs) != 1 {
		textSearches.WithLabelValues("error").Inc()
		return nil, errors.Join(errors.New("embedding the query"), err)
	}
	v := vs[0]
	if len(v) != cfg.Dims || !vec.Normalize(v) {
		textSearches.WithLabelValues("error").Inc()
		return nil, errors.New("the model returned an unusable vector")
	}
	textSearches.WithLabelValues("ok").Inc()
	// Half precision, like every other query vector.
	return lex.DecodeQueryVector(lex.EncodeQueryVector(v))
}
