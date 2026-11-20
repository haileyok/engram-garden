package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/haileyok/cocoon/space"

	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/spaceclient"
)

// Lounge is one agent's place in a lounge: its own presence, its messages,
// and the stream of what others say and do. The agent signs in as its own
// identity (one per session or subagent), not as the account that writes
// memories.
type Lounge struct {
	// Client acts as the agent's own identity.
	Client *spaceclient.Client
	// Space is the lounge's URI.
	Space string
	// AppviewURL and AppviewDID locate the appview that serves the lounge.
	AppviewURL string
	AppviewDID string
	Log        *slog.Logger
	// Lease is how long a presence write counts (default 5m); it's renewed
	// at half that while the lounge runs.
	Lease time.Duration
	// Now is the clock (default time.Now).
	Now func() time.Time
	// Machine is this machine's host name, sent with every message (default
	// os.Hostname).
	Machine string
	// Principal is the DID of the person this agent acts for, and Verify
	// returns the principals another agent declares and whether each
	// confirms it (Principals). With both, received messages say whether
	// their author is confirmed as acting for the same principal.
	Principal string
	Verify    func(ctx context.Context, did string) (map[string]bool, error)

	trustMu sync.Mutex
	trust   map[string]trustEntry
	// writeMu serializes presence writes, so they land in the order they
	// were merged.
	writeMu sync.Mutex

	mu       sync.Mutex
	presence lex.Presence
	wrote    time.Time // when presence was last written
	started  bool
	events   []LoungeEvent
	seq      int64
	cursor   string // the appview's, to resume the stream
	notify   chan struct{}
	conn     string // connection state, for status
}

// LoungeEvent is something that happened in the lounge, as the agent
// received it. Seq numbers them for this process.
type LoungeEvent struct {
	Seq      int64          `json:"seq"`
	Type     string         `json:"type"`
	Presence *AgentPresence `json:"presence,omitempty"`
	Message  *LoungeMessage `json:"message,omitempty"`
	URI      string         `json:"uri,omitempty"`
}

// AgentPresence is an agent in the lounge.
type AgentPresence struct {
	DID    string `json:"did"`
	Handle string `json:"handle,omitempty"`
	Online bool   `json:"online"`
	lex.Presence
}

// LoungeMessage is a message in the lounge.
type LoungeMessage struct {
	URI          string        `json:"uri"`
	CID          string        `json:"cid,omitempty"`
	Author       string        `json:"author"`
	AuthorHandle string        `json:"authorHandle,omitempty"`
	To           []string      `json:"to"`
	Kind         string        `json:"kind,omitempty"`
	Text         string        `json:"text"`
	Reply        *lex.ReplyRef `json:"reply,omitempty"`
	Machine      string        `json:"machine,omitempty"`
	CreatedAt    string        `json:"createdAt"`
	ExpiresAt    string        `json:"expiresAt,omitempty"`
	// SamePrincipal says whether the author is confirmed (ident.link) as
	// an agent of the principal this agent acts for, or is the principal.
	// Absent when unknown.
	SamePrincipal *bool `json:"samePrincipal,omitempty"`
}

type trustEntry struct {
	ok bool
	at time.Time
}

// trustTTL is how long a verification is reused: the spec asks consumers
// to re-verify from time to time.
const trustTTL = 10 * time.Minute

// samePrincipal reports whether did acts for this agent's principal, from
// a cached verification when it's fresh.
func (l *Lounge) samePrincipal(ctx context.Context, did string) *bool {
	if l.Principal == "" || l.Verify == nil {
		return nil
	}
	yes := true
	if did == l.Principal {
		return &yes
	}
	l.trustMu.Lock()
	if e, ok := l.trust[did]; ok && l.now().Sub(e.at) < trustTTL {
		l.trustMu.Unlock()
		return &e.ok
	}
	l.trustMu.Unlock()
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	got, err := l.Verify(vctx, did)
	if err != nil {
		return nil
	}
	ok := got[l.Principal]
	l.trustMu.Lock()
	if l.trust == nil {
		l.trust = map[string]trustEntry{}
	}
	l.trust[did] = trustEntry{ok: ok, at: l.now()}
	l.trustMu.Unlock()
	return &ok
}

// maxEvents bounds the events kept for Events.
const maxEvents = 500

func (l *Lounge) log() *slog.Logger {
	if l.Log == nil {
		return slog.Default()
	}
	return l.Log
}

func (l *Lounge) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Lounge) lease() time.Duration {
	if l.Lease > 0 {
		return l.Lease
	}
	return 5 * time.Minute
}

// machine is the host name messages say they come from.
func (l *Lounge) machine() string {
	if l.Machine != "" {
		return l.Machine
	}
	l.mu.Lock()
	m := l.presence.Machine
	l.mu.Unlock()
	if m != "" {
		return m
	}
	h, _ := os.Hostname()
	return h
}

// DID is the agent's own DID.
func (l *Lounge) DID() string { return l.Client.DID().String() }

func fmtTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// ---- presence ----

// PresenceIn changes the agent's presence. Empty fields keep their value.
type PresenceIn struct {
	State     string `json:"state,omitempty" jsonschema:"starting, working, idle, waiting (on a person) or ended"`
	Title     string `json:"title,omitempty" jsonschema:"a short name for the work"`
	Activity  string `json:"activity,omitempty" jsonschema:"what it's doing right now"`
	Kind      string `json:"kind,omitempty" jsonschema:"session, subagent or agent"`
	Parent    string `json:"parent,omitempty" jsonschema:"the DID of the agent that started this one"`
	SessionID string `json:"sessionId,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	Repo      string `json:"repo,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Machine   string `json:"machine,omitempty"`
	Harness   string `json:"harness,omitempty"`
	Model     string `json:"model,omitempty"`
}

// SetPresence merges a change into the agent's presence and writes it, with
// a fresh lease.
func (l *Lounge) SetPresence(ctx context.Context, in PresenceIn) (AgentPresence, error) {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	l.mu.Lock()
	p := l.presence
	for _, f := range []struct {
		dst *string
		v   string
	}{
		{&p.State, in.State}, {&p.Title, in.Title}, {&p.Activity, in.Activity}, {&p.Kind, in.Kind},
		{&p.Parent, in.Parent}, {&p.SessionID, in.SessionID}, {&p.Cwd, in.Cwd}, {&p.Repo, in.Repo},
		{&p.Branch, in.Branch}, {&p.Machine, in.Machine}, {&p.Harness, in.Harness}, {&p.Model, in.Model},
	} {
		if f.v != "" {
			*f.dst = f.v
		}
	}
	if p.State == "" {
		p.State = lex.StateStarting
	}
	now := l.now()
	if p.StartedAt == "" {
		p.StartedAt = fmtTime(now)
	}
	p.UpdatedAt = fmtTime(now)
	p.LeaseUntil = fmtTime(now.Add(l.lease()))
	l.mu.Unlock()
	if err := p.Normalize(); err != nil {
		return AgentPresence{}, err
	}
	if err := l.putPresence(ctx, p); err != nil {
		return AgentPresence{}, err
	}
	l.mu.Lock()
	l.presence, l.wrote = p, now
	l.mu.Unlock()
	return AgentPresence{DID: l.DID(), Online: p.State != lex.StateEnded, Presence: p}, nil
}

func (l *Lounge) putPresence(ctx context.Context, p lex.Presence) error {
	body := map[string]any{
		"space": l.Space, "repo": l.DID(), "collection": lex.PresenceCollection, "rkey": lex.PresenceRkey,
		"record": p.Record(),
	}
	if err := l.Client.Session.Post(ctx, "com.atproto.space.putRecord", body, nil); err != nil {
		return fmt.Errorf("writing presence: %w", unwrap(err))
	}
	return nil
}

// renew rewrites the presence when half its lease has passed.
func (l *Lounge) renew(ctx context.Context) {
	l.mu.Lock()
	p, wrote := l.presence, l.wrote
	l.mu.Unlock()
	if p.State == "" || p.State == lex.StateEnded || l.now().Sub(wrote) < l.lease()/2 {
		return
	}
	if _, err := l.SetPresence(ctx, PresenceIn{}); err != nil {
		l.log().Warn("renewing presence failed", "err", err)
	}
}

// End marks the agent gone for good.
func (l *Lounge) End(ctx context.Context) error {
	l.mu.Lock()
	written := l.presence.State != ""
	l.mu.Unlock()
	if !written {
		return nil
	}
	_, err := l.SetPresence(ctx, PresenceIn{State: lex.StateEnded})
	return err
}

// ---- messages ----

// SendIn is a message to send.
type SendIn struct {
	To   []string `json:"to,omitempty" jsonschema:"the DIDs of the agents it's for; empty sends it to everyone in the lounge"`
	Text string   `json:"text" jsonschema:"the message"`
	Kind string   `json:"kind,omitempty" jsonschema:"note (no answer needed), request (asks for an answer or work), reply (answers replyTo) or handoff (passes work on); default note, or reply with replyTo"`
	// ReplyTo is the URI of the message this answers.
	ReplyTo string `json:"replyTo,omitempty" jsonschema:"the URI of the message this answers"`
	// TTL is how long the message stays, e.g. 24h (default 72h).
	TTL string `json:"ttl,omitempty" jsonschema:"how long the message is worth keeping, such as 2h (default 72h)"`
}

// SendOut is a sent message.
type SendOut struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

// Send writes a message to the lounge from the agent's own repo.
func (l *Lounge) Send(ctx context.Context, in SendIn) (SendOut, error) {
	now := l.now()
	m := lex.Message{To: append([]string(nil), in.To...), Kind: in.Kind, Text: strings.TrimSpace(in.Text), Machine: l.machine(), CreatedAt: fmtTime(now)}
	ttl := 72 * time.Hour
	if in.TTL != "" {
		d, err := time.ParseDuration(in.TTL)
		if err != nil || d <= 0 {
			return SendOut{}, fmt.Errorf("ttl %q isn't a duration like 2h", in.TTL)
		}
		ttl = d
	}
	m.ExpiresAt = fmtTime(now.Add(ttl))
	if in.ReplyTo != "" {
		parent, err := l.message(ctx, in.ReplyTo)
		if err != nil {
			return SendOut{}, err
		}
		root := in.ReplyTo
		if parent.Reply != nil {
			root = parent.Reply.Root
		}
		m.Reply = &lex.ReplyRef{Root: root, Parent: in.ReplyTo}
		if m.Kind == "" {
			m.Kind = lex.KindReply
		}
		if len(m.To) == 0 {
			m.To = []string{parent.Author}
		}
	}
	if m.Kind == "" {
		m.Kind = lex.KindNote
	}
	if err := m.Validate(); err != nil {
		return SendOut{}, err
	}
	var out SendOut
	body := map[string]any{"space": l.Space, "repo": l.DID(), "collection": lex.MessageCollection, "record": m.Record()}
	if err := l.Client.Session.Post(ctx, "com.atproto.space.createRecord", body, &out); err != nil {
		return out, fmt.Errorf("sending: %w", unwrap(err))
	}
	return out, nil
}

// message finds a message by URI: among those received, else from the
// appview.
func (l *Lounge) message(ctx context.Context, uri string) (LoungeMessage, error) {
	l.mu.Lock()
	for i := len(l.events) - 1; i >= 0; i-- {
		if m := l.events[i].Message; m != nil && m.URI == uri {
			l.mu.Unlock()
			return *m, nil
		}
	}
	l.mu.Unlock()
	ref, err := space.ParseRef(l.Space)
	if err != nil {
		return LoungeMessage{}, err
	}
	rest, ok := strings.CutPrefix(uri, ref.String()+"/")
	parts := strings.Split(rest, "/")
	if !ok || len(parts) != 3 || parts[1] != lex.MessageCollection {
		return LoungeMessage{}, fmt.Errorf("%q is not a message in this lounge", uri)
	}
	out, err := l.Messages(ctx, MessagesIn{All: true, Limit: 200})
	if err != nil {
		return LoungeMessage{}, err
	}
	for _, m := range out.Messages {
		if m.URI == uri {
			return m, nil
		}
	}
	// Not listed (expired, or not read yet): enough to reply to.
	return LoungeMessage{URI: uri, Author: parts[0]}, nil
}

// Delete deletes one of the agent's own messages.
func (l *Lounge) Delete(ctx context.Context, uri string) error {
	ref, err := space.ParseRef(l.Space)
	if err != nil {
		return err
	}
	rest, _ := strings.CutPrefix(uri, ref.String()+"/")
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[0] != l.DID() || parts[1] != lex.MessageCollection {
		return fmt.Errorf("%q is not one of your messages", uri)
	}
	body := map[string]any{"space": l.Space, "repo": l.DID(), "collection": lex.MessageCollection, "rkey": parts[2]}
	return l.Client.Session.Post(ctx, "com.atproto.space.deleteRecord", body, nil)
}

// ---- reads ----

// AgentsIn asks who's in the lounge.
type AgentsIn struct {
	IncludeOffline bool `json:"includeOffline,omitempty" jsonschema:"also list agents that went offline or ended"`
}

// AgentsOut lists the lounge's agents.
type AgentsOut struct {
	Self   string          `json:"self"`
	Agents []AgentPresence `json:"agents"`
}

// Agents lists the lounge's agents, online ones first.
func (l *Lounge) Agents(ctx context.Context, in AgentsIn) (AgentsOut, error) {
	params := url.Values{"space": {l.Space}}
	if in.IncludeOffline {
		params.Set("includeOffline", "true")
	}
	var out AgentsOut
	if err := l.Client.Query(ctx, l.AppviewURL, l.Space, l.AppviewDID, "garden.engram.listPresence", params, &out); err != nil {
		return out, unwrap(err)
	}
	out.Self = l.DID()
	if out.Agents == nil {
		out.Agents = []AgentPresence{}
	}
	return out, nil
}

// MessagesIn asks for the lounge's messages.
type MessagesIn struct {
	// All lists every message, not only those for this agent.
	All   bool   `json:"all,omitempty" jsonschema:"every message in the lounge, not only those for you (and to everyone)"`
	Since string `json:"since,omitempty" jsonschema:"only messages created at or after this RFC 3339 time"`
	Limit int    `json:"limit,omitempty" jsonschema:"at most this many (default 50, max 200)"`
}

// MessagesOut lists messages, oldest first.
type MessagesOut struct {
	Messages []LoungeMessage `json:"messages"`
	Cursor   string          `json:"cursor,omitempty"`
}

// Messages lists messages for this agent (and to everyone), oldest first.
func (l *Lounge) Messages(ctx context.Context, in MessagesIn) (MessagesOut, error) {
	params := url.Values{"space": {l.Space}}
	if !in.All {
		params.Set("to", l.DID())
	}
	if in.Since != "" {
		params.Set("since", in.Since)
	}
	if in.Limit > 0 {
		params.Set("limit", fmt.Sprint(min(in.Limit, 200)))
	}
	var out MessagesOut
	if err := l.Client.Query(ctx, l.AppviewURL, l.Space, l.AppviewDID, "garden.engram.listMessages", params, &out); err != nil {
		return out, unwrap(err)
	}
	if out.Messages == nil {
		out.Messages = []LoungeMessage{}
	}
	for i, m := range out.Messages {
		if m.Author != l.DID() {
			out.Messages[i].SamePrincipal = l.samePrincipal(ctx, m.Author)
		}
	}
	return out, nil
}

// ---- the stream ----

// Run keeps the agent's presence leased and reads the lounge's stream until
// ctx ends, reconnecting with its cursor when the stream drops. On the way
// out it marks the agent ended.
func (l *Lounge) Run(ctx context.Context) {
	l.mu.Lock()
	if l.started {
		l.mu.Unlock()
		return
	}
	l.started = true
	written := l.presence.State != ""
	l.mu.Unlock()
	if !written {
		// Online from the start, even for a client that never sets presence.
		if _, err := l.SetPresence(ctx, PresenceIn{State: lex.StateIdle}); err != nil {
			l.log().Warn("writing presence failed", "err", err)
		}
	}
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				l.renew(ctx)
			}
		}
	}()
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := l.stream(ctx)
		if ctx.Err() != nil {
			break
		}
		l.setConn("reconnecting")
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		l.log().Info("lounge stream ended; reconnecting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
	end, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := l.End(end); err != nil {
		l.log().Warn("marking the agent ended failed", "err", err)
	}
}

func (l *Lounge) setConn(s string) {
	l.mu.Lock()
	l.conn = s
	l.mu.Unlock()
}

// Connection says whether the stream is connected.
func (l *Lounge) Connection() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == "" {
		return "connecting"
	}
	return l.conn
}

func (l *Lounge) stream(ctx context.Context) error {
	l.mu.Lock()
	params := url.Values{"space": {l.Space}, "to": {l.DID()}}
	if l.cursor != "" {
		params.Set("cursor", l.cursor)
	}
	l.mu.Unlock()
	body, err := l.Client.Stream(ctx, l.AppviewURL, l.Space, l.AppviewDID, "garden.engram.subscribeLounge", params)
	if err != nil {
		return err
	}
	defer body.Close()
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var id, typ, data string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if typ != "" {
				l.receive(id, typ, data)
			}
			id, typ, data = "", "", ""
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "id: "):
			id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			typ = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data += strings.TrimPrefix(line, "data: ")
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("stream closed")
}

// receive records one event from the stream.
func (l *Lounge) receive(id, typ, data string) {
	var e struct {
		Type     string         `json:"type"`
		Presence *AgentPresence `json:"presence"`
		Message  *LoungeMessage `json:"message"`
		URI      string         `json:"uri"`
	}
	if data != "" && json.Unmarshal([]byte(data), &e) != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if id != "" {
		l.cursor = id
	}
	switch typ {
	case "ready":
		l.conn = "connected"
		return
	case "reset":
		return
	case "presence":
		if e.Presence == nil {
			return
		}
	case "message":
		if e.Message == nil {
			return
		}
		// A snapshot after a reset repeats what was already received.
		for i := len(l.events) - 1; i >= 0; i-- {
			if m := l.events[i].Message; m != nil && m.URI == e.Message.URI && m.CID == e.Message.CID {
				return
			}
		}
	case "messageDeleted":
	default:
		return
	}
	l.seq++
	l.events = append(l.events, LoungeEvent{Seq: l.seq, Type: typ, Presence: e.Presence, Message: e.Message, URI: e.URI})
	if len(l.events) > maxEvents {
		l.events = append(l.events[:0:0], l.events[len(l.events)-maxEvents:]...)
	}
	if l.notify != nil {
		close(l.notify)
		l.notify = nil
	}
}

// EventsIn asks for what happened after a point.
type EventsIn struct {
	After int64 `json:"after,omitempty" jsonschema:"only events after this seq (0: all kept)"`
	// Wait is how long to wait for one when there are none, in seconds
	// (at most 50).
	Wait int `json:"wait,omitempty" jsonschema:"seconds to wait for an event when none are new (max 50)"`
}

// EventsOut is what happened, and the seq to ask after next.
type EventsOut struct {
	Events     []LoungeEvent `json:"events"`
	Next       int64         `json:"next"`
	Connection string        `json:"connection"`
	// Epoch names this process: seqs from another epoch mean nothing here
	// (the server restarted), so a caller starts again from 0.
	Epoch string `json:"epoch"`
}

// processEpoch is Epoch for this process.
var processEpoch = strconv.FormatInt(time.Now().UnixNano(), 36)

// Events returns the events received after in.After, waiting up to in.Wait
// seconds for one when there are none.
func (l *Lounge) Events(ctx context.Context, in EventsIn) EventsOut {
	wait := time.Duration(min(max(in.Wait, 0), 50)) * time.Second
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		l.mu.Lock()
		var out []LoungeEvent
		for _, e := range l.events {
			if e.Seq > in.After {
				out = append(out, e)
			}
		}
		next, conn := l.seq, l.conn
		if len(out) > 0 || wait == 0 {
			l.mu.Unlock()
			if out == nil {
				out = []LoungeEvent{}
			}
			if conn == "" {
				conn = "connecting"
			}
			for i, e := range out {
				if e.Message != nil && e.Message.Author != l.DID() {
					m := *e.Message
					m.SamePrincipal = l.samePrincipal(ctx, m.Author)
					out[i].Message = &m
				}
			}
			return EventsOut{Events: out, Next: next, Connection: conn, Epoch: processEpoch}
		}
		if l.notify == nil {
			l.notify = make(chan struct{})
		}
		ch := l.notify
		l.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			wait = 0
		case <-deadline.C:
			wait = 0
		}
	}
}
