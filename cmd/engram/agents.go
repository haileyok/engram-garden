package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/haileyok/engram-garden/internal/agent"
)

// Agents with identities of their own (https://ident.link): the principal
// they act for, the lounge they meet in, and the identities themselves.

const agentUsage = `Agent identities (each agent session gets an ATProto account of its own):
  engram principal login [--handle h]       sign in as the person agents act for (their PDS creates and endorses agents)
  engram principal [set --recovery-key did:key --lounge uri --archive uri --stock n]
                                            show or change how agent accounts are made
  engram lounge create <name>               make a lounge (who's online, messages) and make it the agents' lounge
  engram lounge index [--stop]              let the appview serve the lounge (approve in the browser)
  engram agent new [--kind k] [--session id] [--parent did]
                                            hand out an identity (from the stock, else a new account)
  engram agent stock [--fill n]             create identities ahead of time
  engram agent list                         identities handed out, and the stock
  engram agent verify <did>                 the principals an agent declares, and whether each confirms it

As an agent (ENGRAM_IDENTITY names its identity file):
  engram lounge agents [--all]              who's in the lounge
  engram lounge send [--to did]... [--kind k] [--reply-to uri] <text>
  engram lounge messages [--all] [--since t]
`

func (c *cli) principal() (agent.Principal, error) {
	return agent.LoadPrincipal(c.principalPath)
}

// loadOrNewPrincipal reads principal.json, or starts an empty one.
func (c *cli) loadOrNewPrincipal() (agent.Principal, error) {
	p, err := agent.LoadPrincipal(c.principalPath)
	if err != nil {
		if _, serr := os.Stat(c.principalPath); errors.Is(serr, os.ErrNotExist) {
			return agent.Principal{}, nil
		}
		return p, err
	}
	return p, nil
}

func (c *cli) cmdPrincipal(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "login" {
		return c.principalLogin(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "set" {
		return c.principalSet(args[1:])
	}
	if len(args) > 0 {
		return usageError{fmt.Errorf("unknown principal command %q (login, set)", args[0])}
	}
	p, err := c.loadOrNewPrincipal()
	if err != nil {
		return err
	}
	c.print(p, func(w io.Writer) {
		fmt.Fprintf(w, "principal:      %s %s\n", p.Account.Handle, p.Account.DID)
		fmt.Fprintf(w, "recovery key:   %s\n", p.RecoveryKey)
		fmt.Fprintf(w, "lounge:         %s\n", p.Lounge)
		fmt.Fprintf(w, "archive:        %s\n", p.Archive)
		fmt.Fprintf(w, "stock:          %d\n", max(p.Stock, 4))
		if err := p.Check(); err != nil {
			fmt.Fprintf(w, "\n%v\n", err)
		}
	})
	return nil
}

func (c *cli) principalLogin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("principal login", flag.ContinueOnError)
	handle := fs.String("handle", "", "the principal's handle")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	p, err := c.loadOrNewPrincipal()
	if err != nil {
		return err
	}
	h := *handle
	if h == "" {
		h = p.Account.Handle
	}
	if h == "" {
		if h, err = c.ask("The principal's handle (the person agents act for): "); err != nil {
			return err
		}
	}
	acct, granted, err := c.principalSignIn(ctx, strings.TrimPrefix(strings.TrimSpace(h), "@"), c.lineInput())
	if err != nil {
		return err
	}
	old := p.Account
	p.Account, p.Scopes = acct, granted
	if err := p.Save(c.principalPath); err != nil {
		return err
	}
	if old.SessionID != "" && old.SessionID != acct.SessionID {
		_ = c.revoke(ctx, old)
	}
	c.print(map[string]any{"principal": acct.DID, "handle": acct.Handle}, func(w io.Writer) {
		fmt.Fprintf(w, "Signed in as the principal %s (%s).\n", acct.Handle, acct.DID)
		if err := p.Check(); err != nil {
			fmt.Fprintf(w, "Next: engram principal set ... (%v)\n", err)
		}
	})
	return nil
}

func (c *cli) principalSet(args []string) error {
	fs := flag.NewFlagSet("principal set", flag.ContinueOnError)
	recovery := fs.String("recovery-key", "", "a did:key you hold offline, first in every agent's rotation keys")
	lounge := fs.String("lounge", "", "the lounge agents join")
	archive := fs.String("archive", "", "the archive agents keep their sessions in")
	stock := fs.Int("stock", 0, "identities to keep ready")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	p, err := c.loadOrNewPrincipal()
	if err != nil {
		return err
	}
	for _, f := range []struct {
		dst *string
		v   string
	}{{&p.RecoveryKey, *recovery}, {&p.Lounge, *lounge}, {&p.Archive, *archive}} {
		if f.v != "" {
			*f.dst = strings.TrimSpace(f.v)
		}
	}
	if *stock > 0 {
		p.Stock = *stock
	}
	if p.RecoveryKey != "" && !strings.HasPrefix(p.RecoveryKey, "did:key:") {
		return usageError{errors.New("--recovery-key must be a did:key (its public half only)")}
	}
	if err := p.Save(c.principalPath); err != nil {
		return err
	}
	return c.cmdPrincipal(context.Background(), nil)
}

func (c *cli) provisioner(ctx context.Context) (*agent.Provisioner, error) {
	p, err := c.principal()
	if err != nil {
		return nil, err
	}
	if err := p.Check(); err != nil {
		return nil, err
	}
	sess, err := c.principalSession(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("signing in as the principal: %w", err)
	}
	return &agent.Provisioner{Principal: p, Session: sess, Dir: c.identitiesDir}, nil
}

// ---- lounges ----

func (c *cli) cmdLounge(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError{errors.New("lounge needs a command: create, index, agents, send, messages")}
	}
	switch args[0] {
	case "create":
		return c.loungeCreate(ctx, args[1:])
	case "index":
		return c.loungeIndex(ctx, args[1:])
	case "agents":
		return c.loungeAgents(ctx, args[1:])
	case "send":
		return c.loungeSend(ctx, args[1:])
	case "messages":
		return c.loungeMessages(ctx, args[1:])
	}
	return usageError{fmt.Errorf("unknown lounge command %q", args[0])}
}

func (c *cli) loungeCreate(ctx context.Context, args []string) error {
	pos, err := parse(flag.NewFlagSet("lounge create", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{errors.New("usage: engram lounge create <name>")}
	}
	p, err := c.principal()
	if err != nil {
		return err
	}
	sess, err := c.principalSession(ctx, p)
	if err != nil {
		return fmt.Errorf("signing in as the principal: %w", err)
	}
	uri, err := agent.CreateLounge(ctx, sess, pos[0])
	if err != nil {
		return err
	}
	p.Lounge = uri
	if err := p.Save(c.principalPath); err != nil {
		return err
	}
	c.print(map[string]any{"lounge": uri}, func(w io.Writer) {
		fmt.Fprintf(w, "Created the lounge %s; new agents join it.\nNext: engram lounge index (let the appview serve it).\n", uri)
	})
	return nil
}

func (c *cli) loungeIndex(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("lounge index", flag.ContinueOnError)
	stop := fs.Bool("stop", false, "stop the appview serving it")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	p, err := c.principal()
	if err != nil {
		return err
	}
	if p.Lounge == "" {
		return errors.New("no lounge yet: engram lounge create <name>")
	}
	s, err := c.settings()
	if err != nil {
		return err
	}
	mode := "grant"
	if *stop {
		mode = "stop"
	}
	link, err := agent.GrantLink(ctx, s.AppviewURL, c.provisionHTTP, p.Lounge, mode)
	if err != nil {
		return err
	}
	c.print(map[string]any{"lounge": p.Lounge, "link": link}, func(w io.Writer) {
		fmt.Fprintf(w, "Open this and sign in as %s to approve:\n\n  %s\n", p.Account.Handle, link)
	})
	if c.browser != nil && !c.json {
		_ = c.browser(link)
	}
	return nil
}

func (c *cli) lounge(ctx context.Context) (*agent.Lounge, agent.Identity, error) {
	s, err := c.settings()
	if err != nil {
		return nil, agent.Identity{}, err
	}
	return c.openLounge(ctx, s)
}

func (c *cli) loungeAgents(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("lounge agents", flag.ContinueOnError)
	all := fs.Bool("all", false, "include agents that are offline")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	l, _, err := c.lounge(ctx)
	if err != nil {
		return err
	}
	out, err := l.Agents(ctx, agent.AgentsIn{IncludeOffline: *all})
	if err != nil {
		return err
	}
	c.print(out, func(w io.Writer) { writeAgents(w, out) })
	return nil
}

func writeAgents(w io.Writer, out agent.AgentsOut) {
	if len(out.Agents) == 0 {
		fmt.Fprintln(w, "No agents online.")
		return
	}
	for _, a := range out.Agents {
		name := a.Handle
		if name == "" {
			name = a.DID
		}
		me := ""
		if a.DID == out.Self {
			me = " (you)"
		}
		status := a.State
		if !a.Online {
			status = "offline"
		}
		fmt.Fprintf(w, "%s%s  %s  %s\n", name, me, status, a.Title)
		if a.Activity != "" {
			fmt.Fprintf(w, "    %s\n", a.Activity)
		}
		var where []string
		for _, s := range []string{a.Kind, a.Repo, a.Branch, a.Cwd, a.Machine} {
			if s != "" {
				where = append(where, s)
			}
		}
		if a.Parent != "" {
			where = append(where, "parent "+a.Parent)
		}
		if len(where) > 0 {
			fmt.Fprintf(w, "    %s\n", strings.Join(where, " · "))
		}
		fmt.Fprintf(w, "    %s\n", a.DID)
	}
}

func (c *cli) loungeSend(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("lounge send", flag.ContinueOnError)
	var to stringsFlag
	fs.Var(&to, "to", "an agent's DID (repeatable; none: everyone)")
	kind := fs.String("kind", "", "note, request, reply or handoff")
	replyTo := fs.String("reply-to", "", "the URI of the message this answers")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	text := strings.Join(pos, " ")
	if text == "" {
		raw, err := io.ReadAll(c.reader())
		if err != nil {
			return err
		}
		text = string(raw)
	}
	l, _, err := c.lounge(ctx)
	if err != nil {
		return err
	}
	out, err := l.Send(ctx, agent.SendIn{To: to, Text: text, Kind: *kind, ReplyTo: *replyTo})
	if err != nil {
		return err
	}
	c.print(out, func(w io.Writer) { fmt.Fprintf(w, "Sent %s\n", out.URI) })
	return nil
}

func (c *cli) loungeMessages(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("lounge messages", flag.ContinueOnError)
	all := fs.Bool("all", false, "every message, not only those for you")
	since := fs.String("since", "", "only messages created at or after this RFC 3339 time")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	l, _, err := c.lounge(ctx)
	if err != nil {
		return err
	}
	out, err := l.Messages(ctx, agent.MessagesIn{All: *all, Since: *since})
	if err != nil {
		return err
	}
	c.print(out, func(w io.Writer) {
		if len(out.Messages) == 0 {
			fmt.Fprintln(w, "No messages.")
		}
		for _, m := range out.Messages {
			from := m.AuthorHandle
			if from == "" {
				from = m.Author
			}
			to := "everyone"
			if len(m.To) > 0 {
				to = strings.Join(m.To, ", ")
			}
			fmt.Fprintf(w, "%s  %s → %s  [%s]\n  %s\n  %s\n", m.CreatedAt, from, to, m.Kind, strings.ReplaceAll(m.Text, "\n", "\n  "), m.URI)
		}
	})
	return nil
}

// ---- identities ----

func (c *cli) cmdAgent(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError{errors.New("agent needs a command: new, stock, list, verify")}
	}
	switch args[0] {
	case "new":
		return c.agentNew(ctx, args[1:])
	case "stock":
		return c.agentStock(ctx, args[1:])
	case "list":
		return c.agentList(args[1:])
	case "verify":
		return c.agentVerify(ctx, args[1:])
	}
	return usageError{fmt.Errorf("unknown agent command %q", args[0])}
}

func (c *cli) agentNew(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent new", flag.ContinueOnError)
	kind := fs.String("kind", "session", "session or subagent")
	session := fs.String("session", "", "the harness's session ID")
	parent := fs.String("parent", "", "the DID of the agent starting this one")
	noRefill := fs.Bool("no-refill", false, "don't top up the stock in the background")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	pv, err := c.provisioner(ctx)
	if err != nil {
		return err
	}
	stocked := pv.StockCount() > 0
	id, path, err := pv.Claim(ctx, agent.ClaimIn{Kind: *kind, Session: *session, Parent: *parent})
	if err != nil {
		return err
	}
	if !*noRefill && c.spawnRefill != nil && pv.StockCount() < max(pv.Principal.Stock, 4) {
		c.spawnRefill()
	}
	c.print(map[string]any{"did": id.DID, "handle": id.Handle, "path": path, "lounge": id.Lounge, "fromStock": stocked, "principal": id.Principal}, func(w io.Writer) {
		fmt.Fprintf(w, "%s %s\n%s\n", id.DID, id.Handle, path)
	})
	return nil
}

func (c *cli) agentStock(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent stock", flag.ContinueOnError)
	fill := fs.Int("fill", 0, "how many to keep ready (default: the principal's stock)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	pv, err := c.provisioner(ctx)
	if err != nil {
		return err
	}
	// One filler at a time: concurrent `agent new` runs each start one.
	if err := os.MkdirAll(c.identitiesDir, 0o700); err != nil {
		return err
	}
	unlock, err := tryLock(filepath.Join(c.identitiesDir, "stock.lock"))
	if err != nil {
		c.print(map[string]any{"made": 0, "stock": pv.StockCount(), "note": "another fill is running"}, func(w io.Writer) {
			fmt.Fprintln(w, "Another fill is running.")
		})
		return nil
	}
	defer unlock()
	made, err := pv.Fill(ctx, *fill)
	c.print(map[string]any{"made": made, "stock": pv.StockCount()}, func(w io.Writer) {
		fmt.Fprintf(w, "Made %d; %d ready.\n", made, pv.StockCount())
	})
	return err
}

func (c *cli) agentList(args []string) error {
	if _, err := parse(flag.NewFlagSet("agent list", flag.ContinueOnError), args); err != nil {
		return err
	}
	var ids []agent.Identity
	entries, _ := os.ReadDir(filepath.Join(c.identitiesDir, "claimed"))
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id, err := agent.LoadIdentity(filepath.Join(c.identitiesDir, "claimed", e.Name()))
		if err != nil {
			continue
		}
		id.Password = ""
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].ClaimedAt.After(ids[j].ClaimedAt) })
	pv := &agent.Provisioner{Dir: c.identitiesDir}
	stock := pv.StockCount()
	c.print(map[string]any{"identities": ids, "stock": stock}, func(w io.Writer) {
		for _, id := range ids {
			fmt.Fprintf(w, "%s  %s  %s  %s  %s\n", id.ClaimedAt.Local().Format(time.DateTime), id.Kind, id.DID, id.Handle, id.Session)
		}
		fmt.Fprintf(w, "%d handed out, %d ready.\n", len(ids), stock)
	})
	return nil
}

func (c *cli) agentVerify(ctx context.Context, args []string) error {
	pos, err := parse(flag.NewFlagSet("agent verify", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{errors.New("usage: engram agent verify <did>")}
	}
	got, err := c.verifyAgent(ctx, pos[0])
	if err != nil {
		return err
	}
	c.print(map[string]any{"agent": pos[0], "principals": got}, func(w io.Writer) {
		if len(got) == 0 {
			fmt.Fprintln(w, "It declares no principals.")
		}
		for p, ok := range got {
			state := "unconfirmed (no endorsement)"
			if ok {
				state = "confirmed"
			}
			fmt.Fprintf(w, "agent of %s: %s\n", p, state)
		}
	})
	return nil
}

// tryLock takes a lock file without waiting: an error means someone else
// holds it. A lock older than ten minutes is from a run that died.
func tryLock(path string) (func(), error) {
	for range 2 {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if st, serr := os.Stat(path); serr == nil && time.Since(st.ModTime()) > 10*time.Minute {
			_ = os.Remove(path)
			continue
		}
		return nil, err
	}
	return nil, errors.New("lock is held")
}
