// Package coord is how an operator's agents coordinate through records in a
// shared workspace space (garden.engram.agents.workspace): observations
// bring outside facts in, messages and artifacts cite them, tasks are
// claimed and finished through task events, and a presence record keeps
// one run of each agent active at a time.
//
// Everything but presence is written once. The writer here never edits or
// deletes, stamps every record with the run that produced it, refuses
// messages with text that neither cites a record nor is marked as the
// author's own, checks each citation's quoted words against the record it
// cites, and rewrites a citation of another agent's restated fact to cite
// that fact's original record (listing the messages it came through).
package coord

import (
	"encoding/json"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

const (
	NS                    = "garden.engram.agents"
	SpaceType             = NS + ".workspace"
	ObservationCollection = NS + ".observation"
	MessageCollection     = NS + ".message"
	ArtifactCollection    = NS + ".artifact"
	TaskCollection        = NS + ".task"
	TaskEventCollection   = NS + ".taskEvent"
	PresenceCollection    = NS + ".presence"
	HandoffCollection     = NS + ".handoff"
	// DeclarationCollection and OperatedCollection are public records.
	DeclarationCollection = NS + ".declaration"
	OperatedCollection    = NS + ".operated"

	TextQuoteType   = NS + ".defs#textQuote"
	ByteRangeType   = NS + ".defs#byteRange"
	JSONPointerType = NS + ".defs#jsonPointer"

	// SelfRkey is the record key of presence and declaration records.
	SelfRkey = "self"

	timeFormat = "2006-01-02T15:04:05.000Z"
)

// WriteOnceCollections are the workspace collections whose records are
// never updated or deleted, so citations to them stay valid.
var WriteOnceCollections = []string{
	ObservationCollection, MessageCollection, ArtifactCollection,
	TaskCollection, TaskEventCollection, HandoffCollection,
}

// StrongRef points at one version of a record (com.atproto.repo.strongRef).
type StrongRef struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

// Generator is what produced a record: the agent's run, model, harness and
// device. The agent itself is the record's author.
type Generator struct {
	Model          string     `json:"model,omitempty"`
	Harness        string     `json:"harness,omitempty"`
	HarnessVersion string     `json:"harnessVersion,omitempty"`
	Device         string     `json:"device,omitempty"`
	Run            string     `json:"run"`
	Transcript     *StrongRef `json:"transcript,omitempty"`
}

// NewRun mints a run ID for a working session that is starting.
func NewRun() string { return syntax.NewTIDNow(0).String() }

// Span is a UTF-8 byte range of a record's text, end exclusive.
type Span struct {
	ByteStart int `json:"byteStart"`
	ByteEnd   int `json:"byteEnd"`
}

// Selector says which part of a cited record a citation rests on: a text
// quote, a byte range or a JSON pointer, by Type.
type Selector struct {
	Type string `json:"$type"`
	// Text quote.
	Exact  string `json:"exact,omitempty"`
	Prefix string `json:"prefix,omitempty"`
	Suffix string `json:"suffix,omitempty"`
	// Byte range.
	ByteStart *int `json:"byteStart,omitempty"`
	ByteEnd   *int `json:"byteEnd,omitempty"`
	// JSON pointer.
	Pointer string `json:"pointer,omitempty"`
}

// TextQuote selects words that appear in the cited record's text.
func TextQuote(exact string) *Selector { return &Selector{Type: TextQuoteType, Exact: exact} }

// Citation is part of a record's text resting on another record.
type Citation struct {
	Span     Span        `json:"span"`
	Relation string      `json:"relation"`
	Target   StrongRef   `json:"target"`
	Selector *Selector   `json:"selector,omitempty"`
	Via      []StrongRef `json:"via,omitempty"`
}

// Relations a citation can have to its target.
const (
	RelQuotes      = "quotes"
	RelRestates    = "restates"
	RelDerivedFrom = "derivedFrom"
	RelContradicts = "contradicts"
	RelCorrects    = "corrects"
)

// Basis is part of a record's text that is the author's own.
type Basis struct {
	Span Span   `json:"span"`
	Kind string `json:"kind"`
}

// Source is where an observation came from.
type Source struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// Observation is an outside fact entering the workspace.
type Observation struct {
	Type       string          `json:"$type"`
	Source     Source          `json:"source"`
	Method     string          `json:"method"`
	ObservedAt string          `json:"observedAt"`
	Content    json.RawMessage `json:"content,omitempty"`
	Text       string          `json:"text"`
	Supersedes *StrongRef      `json:"supersedes,omitempty"`
	Generator  *Generator      `json:"generator,omitempty"`
	CreatedAt  string          `json:"createdAt"`
}

// Message is a finding, question, proposal, correction or status.
type Message struct {
	Type      string     `json:"$type"`
	Text      string     `json:"text"`
	Kind      string     `json:"kind"`
	Reply     *ReplyRef  `json:"reply,omitempty"`
	Task      *StrongRef `json:"task,omitempty"`
	Citations []Citation `json:"citations,omitempty"`
	Basis     []Basis    `json:"basis,omitempty"`
	Generator *Generator `json:"generator,omitempty"`
	CreatedAt string     `json:"createdAt"`
}

// ReplyRef places a message in a thread.
type ReplyRef struct {
	Root   StrongRef `json:"root"`
	Parent StrongRef `json:"parent"`
}

// Artifact is produced work. A revision is a new artifact with Previous set.
type Artifact struct {
	Type        string          `json:"$type"`
	Title       string          `json:"title"`
	Content     json.RawMessage `json:"content,omitempty"`
	Text        string          `json:"text,omitempty"`
	Task        *StrongRef      `json:"task,omitempty"`
	Previous    *StrongRef      `json:"previous,omitempty"`
	DerivedFrom []Citation      `json:"derivedFrom,omitempty"`
	Generator   *Generator      `json:"generator,omitempty"`
	CreatedAt   string          `json:"createdAt"`
}

// Task is a unit of work. Its state comes from task events (see Fold).
type Task struct {
	Type             string      `json:"$type"`
	Title            string      `json:"title"`
	Description      string      `json:"description,omitempty"`
	Inputs           []StrongRef `json:"inputs,omitempty"`
	DoneWhen         string      `json:"doneWhen,omitempty"`
	Assignee         string      `json:"assignee,omitempty"`
	RequiresApproval bool        `json:"requiresApproval,omitempty"`
	Generator        *Generator  `json:"generator,omitempty"`
	CreatedAt        string      `json:"createdAt"`
}

// Task events.
const (
	EventClaim    = "claim"
	EventProgress = "progress"
	EventDone     = "done"
	EventRelease  = "release"
	EventBlock    = "block"
	EventApprove  = "approve"
)

// TaskEvent is something that happened to a task.
type TaskEvent struct {
	Type       string     `json:"$type"`
	Task       StrongRef  `json:"task"`
	Event      string     `json:"event"`
	LeaseUntil string     `json:"leaseUntil,omitempty"`
	Note       string     `json:"note,omitempty"`
	Output     *StrongRef `json:"output,omitempty"`
	Generator  *Generator `json:"generator,omitempty"`
	CreatedAt  string     `json:"createdAt"`
}

// Presence is which run of an agent is active, in the agent's own repo.
type Presence struct {
	Type        string  `json:"$type"`
	Run         string  `json:"run"`
	Device      string  `json:"device"`
	LeaseUntil  string  `json:"leaseUntil"`
	HandoffFrom *RunRef `json:"handoffFrom,omitempty"`
	UpdatedAt   string  `json:"updatedAt"`
}

// RunRef is the run a presence took over from.
type RunRef struct {
	Run        string `json:"run"`
	Device     string `json:"device"`
	LeaseUntil string `json:"leaseUntil"`
}

// Handoff is written by a run that stopped because another took over. Its
// record key is ToRun.
type Handoff struct {
	Type      string     `json:"$type"`
	FromRun   string     `json:"fromRun"`
	ToRun     string     `json:"toRun"`
	Generator *Generator `json:"generator,omitempty"`
	CreatedAt string     `json:"createdAt"`
}

// Declaration is an agent's public statement of who operates it.
type Declaration struct {
	Type        string `json:"$type"`
	Operator    string `json:"operator"`
	Description string `json:"description,omitempty"`
	CreatedAt   string `json:"createdAt"`
}

// Operated is an operator's public statement that it runs an agent.
type Operated struct {
	Type      string `json:"$type"`
	Agent     string `json:"agent"`
	CreatedAt string `json:"createdAt"`
}

func formatTime(t time.Time) string { return t.UTC().Format(timeFormat) }

func parseTime(s string) (time.Time, bool) {
	t, err := syntax.ParseDatetimeLenient(s)
	if err != nil {
		return time.Time{}, false
	}
	return t.Time(), true
}
