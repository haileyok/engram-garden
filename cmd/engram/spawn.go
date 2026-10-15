package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/haileyok/engram-garden/internal/agent"
	"github.com/haileyok/engram-garden/internal/coord"
)

// maxChain bounds how many agents deep a chain of operators may go.
const maxChain = 16

// spawnOut is what engram agent spawn reports.
type spawnOut struct {
	Agent struct {
		DID    string `json:"did"`
		Handle string `json:"handle"`
	} `json:"agent"`
	Operator    string   `json:"operator,omitempty"`
	Chain       []string `json:"chain,omitempty"`
	RecoveryKey string   `json:"recoveryKey,omitempty"`
	Verified    bool     `json:"verified"`
	Problem     string   `json:"problem,omitempty"`
	// Config is the new agent's settings file, KeyFile its sign-in key.
	Config  string `json:"config"`
	KeyFile string `json:"keyFile"`
}

// cmdAgentSpawn creates an agent operated by the account signed in: an
// account on the same PDS with no password, signing in with a key generated
// here; its declaration naming this account; this account's operated record
// naming it; and its own settings.
func (c *cli) cmdAgentSpawn(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent spawn", flag.ContinueOnError)
	invite := fs.String("invite", "", "an invite code, when the PDS needs one")
	recoveryKey := fs.String("recovery-key", "", "the did:key to put first in the new agent's rotation keys (default: the one this agent's chain shares)")
	desc := fs.String("description", "", "what the agent does, shown with its declaration")
	email := fs.String("email", "", "the account's email (default: a placeholder under agents.invalid)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{errors.New("engram agent spawn <handle> [--invite code] [--recovery-key did:key] [--description d]")}
	}
	handle, err := syntax.ParseHandle(strings.TrimPrefix(pos[0], "@"))
	if err != nil {
		return usageError{fmt.Errorf("%q isn't a handle", pos[0])}
	}
	s, err := c.settings()
	if err != nil {
		return err
	}
	if err := s.CheckAccount(); err != nil {
		return err
	}
	parent := s.Account.DID
	if _, err := syntax.ParseDID(parent); err != nil {
		return fmt.Errorf("the account signed in has no DID in its settings: run `engram login` again")
	}

	// The recovery key: named, or the one this agent's chain already shares.
	recovery := *recoveryKey
	if recovery != "" {
		if _, err := atcrypto.ParsePublicDIDKey(recovery); err != nil {
			return usageError{fmt.Errorf("--recovery-key %q isn't a did:key", recovery)}
		}
	} else {
		isAgent, err := coord.IsAgent(ctx, c.directory(), nil, parent)
		if err != nil {
			return fmt.Errorf("checking whether %s is an agent: %w", parent, err)
		}
		if !isAgent {
			return usageError{fmt.Errorf("%s isn't an agent, so name the key that can take the new agent back: --recovery-key <your offline did:key>", parent)}
		}
		ch, err := coord.VerifyChain(ctx, c.directory(), nil, c.rotationKeys, parent, maxChain)
		if err != nil {
			return fmt.Errorf("this agent's own chain of operators doesn't verify, so it can't pass its recovery key on: %w", err)
		}
		recovery = ch.RecoveryKey
	}

	id, err := c.directory().LookupDID(ctx, syntax.DID(parent))
	if err != nil {
		return fmt.Errorf("resolving %s: %w", parent, err)
	}
	host := id.PDSEndpoint()
	if host == "" {
		return fmt.Errorf("%s has no PDS", parent)
	}

	// The new agent's files. Nothing is left behind if creating the account
	// fails; once it exists, its key and settings are kept.
	dir := filepath.Join(c.children(), handle.String())
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("%s already exists: an agent with that handle was spawned here before", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	created := false
	defer func() {
		if !created {
			_ = os.RemoveAll(dir)
		}
	}()
	key, err := agent.NewSignInKey()
	if err != nil {
		return err
	}
	keyFile := filepath.Join(dir, "key")
	if err := agent.SaveSignInKey(keyFile, key); err != nil {
		return err
	}
	pub, err := key.PublicKey()
	if err != nil {
		return err
	}
	mail := *email
	if mail == "" {
		mail = handle.String() + "@agents.invalid"
	}
	did, err := c.createAccount(ctx, host, agent.NewAccount{
		Handle: handle.String(), Email: mail, InviteCode: *invite, RecoveryKey: recovery, AuthKey: pub.DIDKey(),
	})
	if err != nil {
		return err
	}
	created = true
	child := agent.Account{Handle: handle.String(), DID: did, SignIn: agent.SignInKey, KeyFile: keyFile}
	config := filepath.Join(dir, "config.json")
	cs := agent.Settings{Account: child, AppviewURL: s.AppviewURL, AppviewDID: s.AppviewDID, Embed: s.Embed}
	if err := cs.Save(config); err != nil {
		return fmt.Errorf("created %s, but saving its settings failed: %w", did, err)
	}

	// The pair of records: the child names this account, which claims it.
	api, granted, end, err := c.keySignInFor(ctx, child, coord.DeclareScopes)
	if err != nil {
		return fmt.Errorf("created %s, but it couldn't sign in with its key: %w", did, err)
	}
	if !coord.GrantsRepoWrite(granted, coord.DeclarationCollection, "create", "update") {
		end()
		return fmt.Errorf("created %s, but its PDS didn't grant writing its declaration (granted: %s)", did, strings.Join(granted, " "))
	}
	err = coord.Declare(ctx, api, parent, *desc)
	end()
	if err != nil {
		return fmt.Errorf("created %s, but writing its declaration failed: %w", did, err)
	}
	papi, pend, err := c.publicSession(ctx, "", coord.OperateScopes, coord.OperatedCollection, "create")
	if err != nil {
		return fmt.Errorf("created %s, but signing in to claim it failed (retry with `engram agent operate %s`): %w", did, did, err)
	}
	_, err = coord.Operate(ctx, papi, did)
	pend()
	if err != nil {
		return fmt.Errorf("created %s, but claiming it failed (retry with `engram agent operate %s`): %w", did, did, err)
	}

	out := spawnOut{Config: config, KeyFile: keyFile}
	out.Agent.DID, out.Agent.Handle = did, handle.String()
	if ch, err := coord.VerifyChain(ctx, c.directory(), nil, c.rotationKeys, did, maxChain); err != nil {
		out.Problem = err.Error()
	} else {
		out.Verified, out.Chain, out.Operator, out.RecoveryKey = true, ch.Agents, ch.Operator, ch.RecoveryKey
	}
	c.print(out, func(w io.Writer) {
		fmt.Fprintf(w, "Created %s (%s), operated by %s.\n", handle, did, parent)
		if out.Verified {
			fmt.Fprintf(w, "Verified: %s, operated by %s; recovery key %s.\n", strings.Join(out.Chain, " → "), out.Operator, out.RecoveryKey)
		} else {
			fmt.Fprintf(w, "Not verified: %s\n", out.Problem)
		}
		fmt.Fprintf(w, "\nIts sign-in key and settings are in %s.\n", dir)
		fmt.Fprintf(w, "Run as it:\n  ENGRAM_CONFIG_DIR=%s engram-mcp\n", dir)
		fmt.Fprintf(w, "A space's authority lets it in:\n  engram members add %s --space <space>\n", handle)
	})
	return nil
}

func (c *cli) createAccount(ctx context.Context, host string, in agent.NewAccount) (string, error) {
	if c.newAccount != nil {
		return c.newAccount(ctx, host, in)
	}
	return agent.CreateKeyAccount(ctx, nil, host, in)
}

// children is where spawned agents' files go.
func (c *cli) children() string {
	if c.childrenDir != "" {
		return c.childrenDir
	}
	return filepath.Join(filepath.Dir(c.configPath), "agents")
}
