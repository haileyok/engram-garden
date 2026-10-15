package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/haileyok/engram-garden/internal/spaceclient"
)

// isXRPCError reports whether err is an XRPC error with one of the names,
// from either a PDS session (atclient) or a space client.
func isXRPCError(err error, names ...string) bool {
	if spaceclient.IsError(err, names...) {
		return true
	}
	var ae *atclient.APIError
	if !errors.As(err, &ae) {
		return false
	}
	for _, n := range names {
		if ae.Name == n {
			return true
		}
	}
	return false
}

// Declare writes an agent's public declaration naming its operator, in the
// agent's own repo (session is the agent's). It replaces an earlier one.
func Declare(ctx context.Context, session *atclient.APIClient, operator, description string) error {
	if _, err := syntax.ParseDID(operator); err != nil {
		return fmt.Errorf("operator: %w", err)
	}
	d := Declaration{Type: DeclarationCollection, Operator: operator, Description: description, CreatedAt: formatTime(time.Now())}
	body := map[string]any{"repo": session.AccountDID.String(), "collection": DeclarationCollection, "rkey": SelfRkey, "record": d}
	if err := session.Post(ctx, "com.atproto.repo.putRecord", body, nil); err != nil {
		return fmt.Errorf("writing declaration: %w", err)
	}
	return nil
}

// Operate writes an operator's public record that it runs an agent
// (session is the operator's).
func Operate(ctx context.Context, session *atclient.APIClient, agent string) error {
	if _, err := syntax.ParseDID(agent); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	o := Operated{Type: OperatedCollection, Agent: agent, CreatedAt: formatTime(time.Now())}
	body := map[string]any{"repo": session.AccountDID.String(), "collection": OperatedCollection, "record": o}
	if err := session.Post(ctx, "com.atproto.repo.createRecord", body, nil); err != nil {
		return fmt.Errorf("writing operated record: %w", err)
	}
	return nil
}

// VerifyOperator checks an agent's operator both ways: the agent's
// declaration names the operator, and the operator's repo has an operated
// record naming the agent. It returns the operator's DID.
func VerifyOperator(ctx context.Context, dir identity.Directory, hc *http.Client, agent string) (string, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	var decl struct {
		Value Declaration `json:"value"`
	}
	if err := getPublic(ctx, dir, hc, agent, "com.atproto.repo.getRecord", url.Values{"collection": {DeclarationCollection}, "rkey": {SelfRkey}}, &decl); err != nil {
		return "", fmt.Errorf("%s has no agent declaration: %w", agent, err)
	}
	op := decl.Value.Operator
	if _, err := syntax.ParseDID(op); err != nil {
		return "", fmt.Errorf("%s's declaration has no valid operator", agent)
	}
	cursor := ""
	for {
		params := url.Values{"collection": {OperatedCollection}, "limit": {"100"}}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var out struct {
			Records []struct {
				Value Operated `json:"value"`
			} `json:"records"`
			Cursor string `json:"cursor"`
		}
		if err := getPublic(ctx, dir, hc, op, "com.atproto.repo.listRecords", params, &out); err != nil {
			return "", fmt.Errorf("reading %s's operated records: %w", op, err)
		}
		for _, r := range out.Records {
			if r.Value.Agent == agent {
				return op, nil
			}
		}
		if out.Cursor == "" || len(out.Records) == 0 {
			return "", fmt.Errorf("%s declares %s as its operator, but %s doesn't claim it back", agent, op, op)
		}
		cursor = out.Cursor
	}
}

// getPublic reads from a DID's public repo on its PDS, without auth.
func getPublic(ctx context.Context, dir identity.Directory, hc *http.Client, did, nsid string, params url.Values, out any) error {
	d, err := syntax.ParseDID(did)
	if err != nil {
		return err
	}
	id, err := dir.LookupDID(ctx, d)
	if err != nil {
		return err
	}
	host := id.PDSEndpoint()
	if host == "" {
		return fmt.Errorf("%s has no PDS", did)
	}
	params.Set("repo", did)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(host, "/")+"/xrpc/"+nsid+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var xe spaceclient.Error
		_ = json.NewDecoder(resp.Body).Decode(&xe)
		xe.Status = resp.StatusCode
		return &xe
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// AgentScopes are the OAuth scopes for one agent session in an operator's
// workspaces: read and create the write-once collections, and keep its
// presence record. The PDS refuses any other update or delete, whatever
// the runtime does. There is no public-repo scope: declarations are
// written once when the agent is set up.
func AgentScopes(authority string) []string {
	q := "?authority=" + authority
	for _, c := range WriteOnceCollections {
		q += "&collection=" + c
	}
	return []string{
		"atproto",
		"space:" + SpaceType + q + "&action=read&action=create",
		"space:" + SpaceType + "?authority=" + authority + "&collection=" + PresenceCollection + "&action=read&action=create&action=update",
	}
}
