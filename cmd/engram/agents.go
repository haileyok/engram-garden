package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/haileyok/engram-garden/internal/agent"
	"github.com/haileyok/engram-garden/internal/coord"
)

// agentOut is what the engram agent commands report.
type agentOut struct {
	Agent    string `json:"agent"`
	Operator string `json:"operator,omitempty"`
	// Created: operate wrote an operated record (false: there was one).
	Created *bool `json:"created,omitempty"`
	// Verified: the agent's declaration and the operator's operated record
	// name each other.
	Verified bool `json:"verified"`
	// Problem is why they don't, when they don't.
	Problem string `json:"problem,omitempty"`
	// Chain (verify) is the agent and the agents operating it, nearest
	// first; Operator is then the person at the top, and RecoveryKey the
	// rotation key they all have first.
	Chain       []string `json:"chain,omitempty"`
	RecoveryKey string   `json:"recoveryKey,omitempty"`
}

const agentUsage = "engram agent declare --operator <handle|DID> [--description d] [--as handle] | operate <agent> [--as handle] | verify <agent> | spawn <handle> [--invite code] [--recovery-key did:key]"

// cmdAgent pairs an agent account with its operator: the agent's public
// declaration names the operator, and the operator's public operated record
// names the agent.
func (c *cli) cmdAgent(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError{errors.New(agentUsage)}
	}
	switch args[0] {
	case "declare":
		return c.cmdAgentDeclare(ctx, args[1:])
	case "operate":
		return c.cmdAgentOperate(ctx, args[1:])
	case "verify":
		return c.cmdAgentVerify(ctx, args[1:])
	case "spawn":
		return c.cmdAgentSpawn(ctx, args[1:])
	}
	return usageError{fmt.Errorf("unknown subcommand %q: %s", args[0], agentUsage)}
}

func (c *cli) cmdAgentDeclare(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent declare", flag.ContinueOnError)
	operator := fs.String("operator", "", "the operator's handle or DID")
	desc := fs.String("description", "", "what the agent does, shown with its declaration")
	as := fs.String("as", "", "the agent's handle (default: the account signed in)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *operator == "" || len(pos) > 0 {
		return usageError{errors.New("engram agent declare --operator <handle|DID> [--description d] [--as handle]")}
	}
	opDID, err := c.resolveDID(ctx, *operator)
	if err != nil {
		return err
	}
	api, end, err := c.publicSession(ctx, *as, coord.DeclareScopes, coord.DeclarationCollection, "create", "update")
	if err != nil {
		return err
	}
	defer end()
	if err := coord.Declare(ctx, api, opDID, *desc); err != nil {
		return err
	}
	out := agentOut{Agent: api.AccountDID.String(), Operator: opDID}
	if _, err := coord.VerifyOperator(ctx, c.directory(), nil, out.Agent); err == nil {
		out.Verified = true
	} else {
		out.Problem = err.Error()
	}
	c.print(out, func(w io.Writer) {
		fmt.Fprintf(w, "%s declares %s as its operator.\n", out.Agent, out.Operator)
		if out.Verified {
			fmt.Fprintln(w, "Verified: the operator claims it back.")
			return
		}
		fmt.Fprintf(w, "\nNext, the operator claims it back, signed in as %s:\n  engram agent operate %s\n", out.Operator, out.Agent)
	})
	return nil
}

func (c *cli) cmdAgentOperate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent operate", flag.ContinueOnError)
	as := fs.String("as", "", "the operator's handle (default: the account signed in)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{errors.New("engram agent operate <agent handle|DID> [--as handle]")}
	}
	agentDID, err := c.resolveDID(ctx, pos[0])
	if err != nil {
		return err
	}
	api, end, err := c.publicSession(ctx, *as, coord.OperateScopes, coord.OperatedCollection, "create")
	if err != nil {
		return err
	}
	defer end()
	created, err := coord.Operate(ctx, api, agentDID)
	if err != nil {
		return err
	}
	me := api.AccountDID.String()
	out := agentOut{Agent: agentDID, Operator: me, Created: &created}
	if got, err := coord.VerifyOperator(ctx, c.directory(), nil, agentDID); err != nil {
		out.Problem = err.Error()
	} else if got != me {
		out.Problem = fmt.Sprintf("%s declares %s as its operator, not %s", agentDID, got, me)
	} else {
		out.Verified = true
	}
	c.print(out, func(w io.Writer) {
		fmt.Fprintf(w, "%s operates %s", me, agentDID)
		if !created {
			fmt.Fprint(w, " (already recorded)")
		}
		fmt.Fprintln(w, ".")
		if out.Verified {
			fmt.Fprintln(w, "Verified: its declaration names you.")
			return
		}
		fmt.Fprintf(w, "Not verified yet: %s\nThe agent declares you, signed in as itself:\n  engram agent declare --operator %s\n", out.Problem, me)
	})
	return nil
}

func (c *cli) cmdAgentVerify(ctx context.Context, args []string) error {
	pos, err := parse(flag.NewFlagSet("agent verify", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{errors.New("engram agent verify <agent handle|DID>")}
	}
	agentDID, err := c.resolveDID(ctx, pos[0])
	if err != nil {
		return err
	}
	ch, err := coord.VerifyChain(ctx, c.directory(), nil, c.rotationKeys, agentDID, maxChain)
	if err != nil {
		return err
	}
	out := agentOut{Agent: agentDID, Operator: ch.Operator, Verified: true, Chain: ch.Agents, RecoveryKey: ch.RecoveryKey}
	c.print(out, func(w io.Writer) {
		fmt.Fprintf(w, "%s, operated by %s: every link names the next both ways.\n", strings.Join(ch.Agents, " → "), ch.Operator)
		fmt.Fprintf(w, "Each agent's first rotation key is %s.\n", ch.RecoveryKey)
	})
	return nil
}

// publicSession is a session that can write one collection of the account's
// public repo. A password sign-in can already; an OAuth sign-in only reaches
// memory spaces, so this signs in once more for just that collection, and
// the returned end revokes it.
func (c *cli) publicSession(ctx context.Context, as string, scopes []string, collection string, actions ...string) (*atclient.APIClient, func(), error) {
	s, err := c.settings()
	if err != nil {
		return nil, nil, err
	}
	acct := s.Account
	if as == "" {
		as = acct.Handle
	}
	if as == "" {
		return nil, nil, usageError{errors.New("no account signed in: name it with --as <handle>")}
	}
	self := as == acct.Handle || as == acct.DID
	if acct.SignIn == agent.SignInPassword && self {
		sp, err := c.open(ctx, s)
		if err != nil {
			return nil, nil, err
		}
		return sp.Client.Session, func() {}, nil
	}
	var api *atclient.APIClient
	var granted []string
	var end func()
	switch {
	case acct.SignIn == agent.SignInKey && self:
		// A key account signs in for the task itself: no browser.
		api, granted, end, err = c.keySignInFor(ctx, acct, scopes)
	case c.signInFor == nil:
		return nil, nil, errors.New("signing in isn't available here")
	default:
		api, granted, end, err = c.signInFor(ctx, as, scopes, c.lineInput())
	}
	if err != nil {
		return nil, nil, err
	}
	if !coord.GrantsRepoWrite(granted, collection, actions...) {
		end()
		return nil, nil, fmt.Errorf("the account's server didn't grant writing %s (granted: %s)", collection, strings.Join(granted, " "))
	}
	return api, end, nil
}

func (c *cli) directory() identity.Directory {
	if c.dir == nil {
		return identity.DefaultDirectory()
	}
	return c.dir
}

// resolveDID turns a handle or DID into a DID.
func (c *cli) resolveDID(ctx context.Context, s string) (string, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "@")
	if strings.HasPrefix(s, "did:") {
		d, err := syntax.ParseDID(s)
		if err != nil {
			return "", usageError{fmt.Errorf("%q: %w", s, err)}
		}
		return d.String(), nil
	}
	h, err := syntax.ParseHandle(s)
	if err != nil {
		return "", usageError{fmt.Errorf("%q is neither a handle nor a DID", s)}
	}
	id, err := c.directory().LookupHandle(ctx, h)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", s, err)
	}
	return id.DID.String(), nil
}
