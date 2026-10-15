package coord

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

var (
	sourceKinds  = []string{"url", "file", "command", "api", "human"}
	messageKinds = []string{"finding", "question", "proposal", "correction", "status"}
	basisKinds   = []string{"inference", "opinion", "question", "proposal"}
	relations    = []string{RelQuotes, RelRestates, RelDerivedFrom, RelContradicts, RelCorrects}
)

// Observe records an outside fact. ObservedAt defaults to now.
func (w *Workspace) Observe(ctx context.Context, o Observation) (StrongRef, error) {
	if !slices.Contains(sourceKinds, o.Source.Kind) || o.Source.Value == "" {
		return StrongRef{}, fmt.Errorf("source needs a kind (%v) and a value", sourceKinds)
	}
	if o.Method == "" {
		return StrongRef{}, errors.New("method is required: how it was observed, e.g. the tool")
	}
	if o.Text == "" {
		return StrongRef{}, errors.New("text is required")
	}
	if len(o.Text) > 100000 {
		return StrongRef{}, errors.New("text is too long (max 100000 bytes); keep the full content in a blob")
	}
	if o.Supersedes != nil {
		rec, err := w.Get(ctx, *o.Supersedes)
		if err != nil {
			return StrongRef{}, fmt.Errorf("supersedes: %w", err)
		}
		if rec.Collection != ObservationCollection {
			return StrongRef{}, errors.New("supersedes must point at an observation")
		}
	}
	g, err := w.generator()
	if err != nil {
		return StrongRef{}, err
	}
	now := formatTime(w.now())
	o.Type, o.Generator, o.CreatedAt = ObservationCollection, g, now
	if o.ObservedAt == "" {
		o.ObservedAt = now
	}
	return w.create(ctx, ObservationCollection, "", o)
}

// Posted is a message as written, after its citations were checked and
// rewritten to cite original records.
type Posted struct {
	Ref     StrongRef
	Message Message
}

// Post writes a message. Every part of its text must be covered by a
// citation or a basis entry (whitespace and punctuation between them
// aside), every citation must check out against the record it cites, and
// a citation of a fact another message restated is rewritten to cite that
// message's own evidence, with the message listed in Via.
func (w *Workspace) Post(ctx context.Context, m Message) (Posted, error) {
	if m.Text == "" {
		return Posted{}, errors.New("text is required")
	}
	if len(m.Text) > 30000 {
		return Posted{}, errors.New("text is too long (max 30000 bytes)")
	}
	if !slices.Contains(messageKinds, m.Kind) {
		return Posted{}, fmt.Errorf("kind must be one of %v", messageKinds)
	}
	if err := checkSpans(m.Text, m.Citations, m.Basis); err != nil {
		return Posted{}, err
	}
	if gaps := uncovered(m.Text, m.Citations, m.Basis); len(gaps) > 0 {
		return Posted{}, &UncitedError{Text: m.Text, Spans: gaps}
	}
	cites, err := w.resolveCitations(ctx, m.Text, m.Citations)
	if err != nil {
		return Posted{}, err
	}
	m.Citations = cites
	for _, ref := range []*StrongRef{m.Task, replyPart(m.Reply, true), replyPart(m.Reply, false)} {
		if ref != nil {
			if _, err := w.Get(ctx, *ref); err != nil {
				return Posted{}, fmt.Errorf("%s: %w", ref.URI, err)
			}
		}
	}
	g, err := w.generator()
	if err != nil {
		return Posted{}, err
	}
	m.Type, m.Generator, m.CreatedAt = MessageCollection, g, formatTime(w.now())
	ref, err := w.create(ctx, MessageCollection, "", m)
	if err != nil {
		return Posted{}, err
	}
	return Posted{Ref: ref, Message: m}, nil
}

func replyPart(r *ReplyRef, root bool) *StrongRef {
	if r == nil {
		return nil
	}
	if root {
		return &r.Root
	}
	return &r.Parent
}

// Produce writes an artifact. Its derivedFrom citations are checked like a
// message's, with spans in Text, but needn't cover all of it.
func (w *Workspace) Produce(ctx context.Context, a Artifact) (StrongRef, error) {
	if a.Title == "" {
		return StrongRef{}, errors.New("title is required")
	}
	if len(a.DerivedFrom) > 0 {
		if err := checkSpans(a.Text, a.DerivedFrom, nil); err != nil {
			return StrongRef{}, err
		}
		cites, err := w.resolveCitations(ctx, a.Text, a.DerivedFrom)
		if err != nil {
			return StrongRef{}, err
		}
		a.DerivedFrom = cites
	}
	for _, ref := range []*StrongRef{a.Task, a.Previous} {
		if ref != nil {
			if _, err := w.Get(ctx, *ref); err != nil {
				return StrongRef{}, fmt.Errorf("%s: %w", ref.URI, err)
			}
		}
	}
	g, err := w.generator()
	if err != nil {
		return StrongRef{}, err
	}
	a.Type, a.Generator, a.CreatedAt = ArtifactCollection, g, formatTime(w.now())
	return w.create(ctx, ArtifactCollection, "", a)
}

// CreateTask writes a task. Its inputs must resolve.
func (w *Workspace) CreateTask(ctx context.Context, t Task) (StrongRef, error) {
	if t.Title == "" {
		return StrongRef{}, errors.New("title is required")
	}
	if t.Assignee != "" {
		if _, err := syntax.ParseDID(t.Assignee); err != nil {
			return StrongRef{}, fmt.Errorf("assignee: %w", err)
		}
	}
	for _, in := range t.Inputs {
		if _, err := w.Get(ctx, in); err != nil {
			return StrongRef{}, fmt.Errorf("input %s: %w", in.URI, err)
		}
	}
	g, err := w.generator()
	if err != nil {
		return StrongRef{}, err
	}
	t.Type, t.Generator, t.CreatedAt = TaskCollection, g, formatTime(w.now())
	return w.create(ctx, TaskCollection, "", t)
}

// checkSpans checks every span lies within the text on character
// boundaries, and that relations and basis kinds are known.
func checkSpans(text string, cites []Citation, basis []Basis) error {
	ok := func(s Span) bool {
		return s.ByteStart >= 0 && s.ByteStart < s.ByteEnd && s.ByteEnd <= len(text) &&
			utf8.RuneStart(text[s.ByteStart]) && (s.ByteEnd == len(text) || utf8.RuneStart(text[s.ByteEnd]))
	}
	for i, c := range cites {
		if !ok(c.Span) {
			return fmt.Errorf("citation %d: span %d-%d isn't a range of the text", i, c.Span.ByteStart, c.Span.ByteEnd)
		}
		if !slices.Contains(relations, c.Relation) {
			return fmt.Errorf("citation %d: relation must be one of %v", i, relations)
		}
		if c.Target.URI == "" || c.Target.CID == "" {
			return fmt.Errorf("citation %d: target needs a uri and cid", i)
		}
	}
	for i, b := range basis {
		if !ok(b.Span) {
			return fmt.Errorf("basis %d: span %d-%d isn't a range of the text", i, b.Span.ByteStart, b.Span.ByteEnd)
		}
		if !slices.Contains(basisKinds, b.Kind) {
			return fmt.Errorf("basis %d: kind must be one of %v", i, basisKinds)
		}
	}
	return nil
}
