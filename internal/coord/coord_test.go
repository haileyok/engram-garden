package coord

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/haileyok/engram-garden/internal/spaceclient"
	"github.com/haileyok/engram-garden/internal/spacetest"
)

type fixture struct {
	net        *spacetest.Net
	researcher *Workspace
	writer     *Workspace
	operator   *Workspace
}

func setup(t *testing.T) *fixture {
	t.Helper()
	n := spacetest.NewSpaceOfType(t, SpaceType, "operator", "proj")
	f := &fixture{net: n}
	f.operator = workspace(t, n, n.Authority, "operator-laptop")
	f.researcher = workspace(t, n, n.NewAccount("did:plc:researcher"), "laptop")
	f.writer = workspace(t, n, n.NewAccount("did:plc:writer"), "phone")
	n.AddMember("did:plc:researcher")
	n.AddMember("did:plc:writer")
	return f
}

// workspace is one run of an account's agent on a device.
func workspace(t *testing.T, n *spacetest.Net, a *spacetest.Account, device string) *Workspace {
	t.Helper()
	c, err := spaceclient.New(n.Session(a), n.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &Workspace{
		Client:    c,
		Space:     n.Space,
		Generator: Generator{Model: "test-model", Harness: "coordtest", Device: device, Run: NewRun()},
	}
}

func (f *fixture) observe(t *testing.T, w *Workspace, text string) StrongRef {
	t.Helper()
	ref, err := w.Observe(context.Background(), Observation{
		Source: Source{Kind: "url", Value: "https://example.com/report"},
		Method: "web_fetch",
		Text:   text,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

const report = "Quarterly report. Churn rose to 4.1% in Q3, from 3.2%. Revenue was flat."

func span(text, part string) Span {
	i := strings.Index(text, part)
	if i < 0 {
		panic("no " + part)
	}
	return Span{ByteStart: i, ByteEnd: i + len(part)}
}

func TestObserveStampsAndResolves(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	ref := f.observe(t, f.researcher, report)
	rec, err := f.writer.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Author != "did:plc:researcher" || rec.Collection != ObservationCollection {
		t.Fatalf("got %+v", rec)
	}
	var o Observation
	if err := rec.Decode(&o); err != nil {
		t.Fatal(err)
	}
	if o.Text != report || o.Generator == nil || o.Generator.Run != f.researcher.Generator.Run || o.Generator.Device != "laptop" {
		t.Fatalf("observation %+v", o)
	}
	if o.CreatedAt == "" || o.ObservedAt == "" {
		t.Fatal("timestamps not set")
	}
}

func TestGetRefusesWrongCID(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ref := f.observe(t, f.researcher, report)
	other := f.observe(t, f.researcher, "something else")
	ref.CID = other.CID
	_, err := f.writer.Get(context.Background(), ref)
	var stale *StaleError
	if !errors.As(err, &stale) {
		t.Fatalf("want StaleError, got %v", err)
	}
}

func TestWritesNeedARun(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.researcher.Generator.Run = ""
	if _, err := f.researcher.Observe(context.Background(), Observation{Source: Source{Kind: "url", Value: "x"}, Method: "m", Text: "t"}); err == nil {
		t.Fatal("wrote without a run")
	}
}

func TestPostRefusesUncitedText(t *testing.T) {
	t.Parallel()
	f := setup(t)
	obs := f.observe(t, f.researcher, report)
	text := "Churn rose to 4.1% in Q3. The CEO resigned."
	_, err := f.researcher.Post(context.Background(), Message{
		Kind: "finding",
		Text: text,
		Citations: []Citation{{
			Span: span(text, "Churn rose to 4.1% in Q3"), Relation: "quotes", Target: obs,
			Selector: TextQuote("Churn rose to 4.1% in Q3"),
		}},
	})
	var unc *UncitedError
	if !errors.As(err, &unc) || len(unc.Spans) != 1 || text[unc.Spans[0].ByteStart:unc.Spans[0].ByteEnd] != "The CEO resigned" {
		t.Fatalf("want the CEO sentence uncited, got %v", err)
	}
}

func TestPostRefusesPhantomCitation(t *testing.T) {
	t.Parallel()
	f := setup(t)
	obs := f.observe(t, f.researcher, report)
	text := "Churn fell to 2%."
	_, err := f.researcher.Post(context.Background(), Message{
		Kind: "finding", Text: text,
		Citations: []Citation{{Span: span(text, text), Relation: "restates", Target: obs, Selector: TextQuote("Churn fell to 2%")}},
	})
	var ce *CitationError
	if !errors.As(err, &ce) || ce.State != StateMismatch {
		t.Fatalf("want mismatch, got %v", err)
	}
}

func TestQuoteMustReproduceTheWords(t *testing.T) {
	t.Parallel()
	f := setup(t)
	obs := f.observe(t, f.researcher, report)
	text := "Churn rose to 5% in Q3"
	_, err := f.researcher.Post(context.Background(), Message{
		Kind: "finding", Text: text,
		Citations: []Citation{{Span: span(text, text), Relation: "quotes", Target: obs, Selector: TextQuote("Churn rose to 4.1% in Q3")}},
	})
	var ce *CitationError
	if !errors.As(err, &ce) || ce.State != StateMismatch {
		t.Fatalf("want mismatch, got %v", err)
	}
}

func TestPrefixAndSuffixPickTheOccurrence(t *testing.T) {
	t.Parallel()
	f := setup(t)
	obs := f.observe(t, f.researcher, "In 2025 churn was 3%. In 2026 churn was 4%.")
	text := "Churn was 4%."
	sel := TextQuote("churn was")
	sel.Prefix, sel.Suffix = "2027 ", ""
	_, err := f.researcher.Post(context.Background(), Message{
		Kind: "finding", Text: text,
		Citations: []Citation{{Span: span(text, "Churn was 4%"), Relation: "restates", Target: obs, Selector: sel}},
	})
	if err == nil {
		t.Fatal("a prefix that doesn't occur matched")
	}
	sel.Prefix = "2026 "
	if _, err := f.researcher.Post(context.Background(), Message{
		Kind: "finding", Text: text,
		Citations: []Citation{{Span: span(text, "Churn was 4%"), Relation: "restates", Target: obs, Selector: sel}},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPostRefusesStaleTarget(t *testing.T) {
	t.Parallel()
	f := setup(t)
	obs := f.observe(t, f.researcher, report)
	other := f.observe(t, f.researcher, "other")
	obs.CID = other.CID
	text := "Churn rose to 4.1% in Q3"
	_, err := f.researcher.Post(context.Background(), Message{
		Kind: "finding", Text: text,
		Citations: []Citation{{Span: span(text, text), Relation: "quotes", Target: obs, Selector: TextQuote(text)}},
	})
	var ce *CitationError
	if !errors.As(err, &ce) || ce.State != StateStale {
		t.Fatalf("want stale, got %v", err)
	}
}

// The design's worked example: the writer reuses the researcher's figure
// and ends up citing the observation, with the researcher's message in via.
func TestCiteThroughToTheObservation(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	obs := f.observe(t, f.researcher, report)
	m1text := "The report puts Q3 churn at 4.1%, up from 3.2%. I think the pricing change drove it."
	rootSel := TextQuote("Churn rose to 4.1% in Q3, from 3.2%")
	m1, err := f.researcher.Post(ctx, Message{
		Kind: "finding", Text: m1text,
		Citations: []Citation{{Span: span(m1text, "The report puts Q3 churn at 4.1%, up from 3.2%"), Relation: "restates", Target: obs, Selector: rootSel}},
		Basis:     []Basis{{Span: span(m1text, "I think the pricing change drove it"), Kind: "inference"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	m2text := "Q3 churn was 4.1%."
	posted, err := f.writer.Post(ctx, Message{
		Kind: "finding", Text: m2text,
		Citations: []Citation{{Span: span(m2text, "Q3 churn was 4.1%"), Relation: "restates", Target: m1.Ref, Selector: TextQuote("Q3 churn at 4.1%")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := posted.Message.Citations[0]
	if c.Target != obs {
		t.Fatalf("target %v, want the observation %v", c.Target, obs)
	}
	if c.Selector == nil || c.Selector.Exact != rootSel.Exact {
		t.Fatalf("selector %+v, want the researcher's evidence", c.Selector)
	}
	if len(c.Via) != 1 || c.Via[0] != m1.Ref {
		t.Fatalf("via %v, want [%v]", c.Via, m1.Ref)
	}
	// What was stored matches what Post returned.
	rec, err := f.operator.Get(ctx, posted.Ref)
	if err != nil {
		t.Fatal(err)
	}
	var stored Message
	if err := rec.Decode(&stored); err != nil {
		t.Fatal(err)
	}
	if stored.Citations[0].Target != obs || len(stored.Citations[0].Via) != 1 {
		t.Fatalf("stored %+v", stored.Citations[0])
	}

	// A third agent relaying the writer's message still lands on the
	// observation, with both messages in via, nearest first.
	m3text := "Churn: 4.1%"
	p3, err := f.operator.Post(ctx, Message{
		Kind: "finding", Text: m3text,
		Citations: []Citation{{Span: span(m3text, m3text), Relation: "restates", Target: posted.Ref, Selector: TextQuote("Q3 churn was 4.1%")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	c3 := p3.Message.Citations[0]
	if c3.Target != obs || len(c3.Via) != 2 || c3.Via[0] != posted.Ref || c3.Via[1] != m1.Ref {
		t.Fatalf("relay citation %+v", c3)
	}
}

func TestCitingAnInferenceKeepsTheMessage(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	obs := f.observe(t, f.researcher, report)
	m1text := "Churn rose to 4.1% in Q3. I think the pricing change drove it."
	m1, err := f.researcher.Post(ctx, Message{
		Kind: "finding", Text: m1text,
		Citations: []Citation{{Span: span(m1text, "Churn rose to 4.1% in Q3"), Relation: "quotes", Target: obs, Selector: TextQuote("Churn rose to 4.1% in Q3")}},
		Basis:     []Basis{{Span: span(m1text, "I think the pricing change drove it"), Kind: "inference"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := "The researcher thinks pricing drove it."
	p, err := f.writer.Post(ctx, Message{
		Kind: "finding", Text: text,
		Citations: []Citation{{Span: span(text, text[:len(text)-1]), Relation: "restates", Target: m1.Ref, Selector: TextQuote("the pricing change drove it")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c := p.Message.Citations[0]; c.Target != m1.Ref || len(c.Via) != 0 {
		t.Fatalf("citation %+v, want the message itself", c)
	}
}

func TestQuotingAQuoteNarrowsTheSelector(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	obs := f.observe(t, f.researcher, report)
	m1text := "Churn rose to 4.1% in Q3, from 3.2%"
	m1, err := f.researcher.Post(ctx, Message{
		Kind: "finding", Text: m1text,
		Citations: []Citation{{Span: span(m1text, m1text), Relation: "quotes", Target: obs, Selector: TextQuote(m1text)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := "Churn rose to 4.1% in Q3"
	p, err := f.writer.Post(ctx, Message{
		Kind: "finding", Text: text,
		Citations: []Citation{{Span: span(text, text), Relation: "quotes", Target: m1.Ref, Selector: TextQuote(text)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := p.Message.Citations[0]
	if c.Target != obs || c.Relation != "quotes" || c.Selector.Exact != text {
		t.Fatalf("citation %+v", c)
	}
}

func TestCiteThroughRefusesSplitQuotes(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	obs := f.observe(t, f.researcher, report)
	m1text := "Churn rose to 4.1% in Q3. Revenue was flat."
	m1, err := f.researcher.Post(ctx, Message{
		Kind: "finding", Text: m1text,
		Citations: []Citation{
			{Span: span(m1text, "Churn rose to 4.1% in Q3"), Relation: "quotes", Target: obs, Selector: TextQuote("Churn rose to 4.1% in Q3")},
			{Span: span(m1text, "Revenue was flat"), Relation: "quotes", Target: obs, Selector: TextQuote("Revenue was flat")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := "Churn up, revenue flat."
	_, err = f.writer.Post(ctx, Message{
		Kind: "finding", Text: text,
		Citations: []Citation{{Span: span(text, "Churn up, revenue flat"), Relation: "restates", Target: m1.Ref, Selector: TextQuote("in Q3. Revenue")}},
	})
	if err == nil {
		t.Fatal("a quote spanning two citations was accepted")
	}
}

func TestArtifactCitationsAreChecked(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := context.Background()
	obs := f.observe(t, f.researcher, report)
	text := "Summary: churn rose to 4.1%."
	_, err := f.writer.Produce(ctx, Artifact{
		Title: "Summary", Text: text,
		DerivedFrom: []Citation{{Span: span(text, "churn rose to 4.1%"), Relation: "restates", Target: obs, Selector: TextQuote("revenue doubled")}},
	})
	if err == nil {
		t.Fatal("a phantom citation in an artifact was accepted")
	}
	if _, err := f.writer.Produce(ctx, Artifact{
		Title: "Summary", Text: text,
		DerivedFrom: []Citation{{Span: span(text, "churn rose to 4.1%"), Relation: "restates", Target: obs, Selector: TextQuote("Churn rose to 4.1%")}},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestListAcrossMembers(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.observe(t, f.researcher, "one")
	f.observe(t, f.writer, "two")
	recs, err := f.operator.List(context.Background(), ObservationCollection)
	if err != nil {
		t.Fatal(err)
	}
	authors := map[string]bool{}
	for _, r := range recs {
		authors[r.Author] = true
	}
	if len(recs) != 2 || !authors["did:plc:researcher"] || !authors["did:plc:writer"] {
		t.Fatalf("got %+v", recs)
	}
}

func TestNowIsInjectable(t *testing.T) {
	t.Parallel()
	f := setup(t)
	at := time.Date(2026, 10, 14, 16, 2, 11, 0, time.UTC)
	f.researcher.Now = func() time.Time { return at }
	ref := f.observe(t, f.researcher, report)
	rec, _ := f.researcher.Get(context.Background(), ref)
	var o Observation
	_ = rec.Decode(&o)
	if o.ObservedAt != "2026-10-14T16:02:11.000Z" {
		t.Fatalf("observedAt %s", o.ObservedAt)
	}
}
