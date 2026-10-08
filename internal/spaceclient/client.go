// Package spaceclient reads and writes ATProto Spaces as one account: it
// exchanges the account's delegation tokens for space credentials and signs
// requests with the key each credential is bound to.
package spaceclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/haileyok/cocoon/space"
)

// renewBefore is how long before expiry a cached credential is replaced.
const renewBefore = 90 * time.Second

// Error is an XRPC error response.
type Error struct {
	Status  int
	Name    string `json:"error"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%d %s: %s", e.Status, e.Name, e.Message)
	}
	return fmt.Sprintf("%d %s", e.Status, e.Name)
}

// IsError reports whether err is an XRPC error with one of the given names.
func IsError(err error, names ...string) bool {
	var xe *Error
	if !errors.As(err, &xe) {
		return false
	}
	for _, n := range names {
		if xe.Name == n {
			return true
		}
	}
	return false
}

// Delegator mints delegation tokens: proof, from a user's PDS, that this
// client acts for that user in a space.
type Delegator interface {
	DelegationToken(ctx context.Context, spaceURI string) (string, error)
}

// SessionDelegator asks the session's own PDS for delegation tokens.
type SessionDelegator struct{ Session *atclient.APIClient }

// DelegationToken implements Delegator.
func (d SessionDelegator) DelegationToken(ctx context.Context, spaceURI string) (string, error) {
	var tok struct {
		Token string `json:"token"`
	}
	if err := d.Session.Get(ctx, "com.atproto.space.getDelegationToken", map[string]any{"space": spaceURI}, &tok); err != nil {
		return "", fmt.Errorf("getDelegationToken: %w", err)
	}
	return tok.Token, nil
}

// Client reads spaces for a user, and writes as one account when it has a
// session.
type Client struct {
	// Session is authenticated to the account's own PDS. The account's own
	// space writes go through it. Nil for a client that only reads.
	Session *atclient.APIClient
	// Delegator mints the delegation tokens exchanged for credentials.
	Delegator Delegator
	Dir       identity.Directory
	HTTP      *http.Client

	key *atcrypto.PrivateKeyP256

	mu    sync.Mutex
	creds map[string]credential
}

type credential struct {
	jwt string
	exp time.Time
}

// New returns a client for an authenticated session. Each client binds its
// credentials to its own fresh P-256 key.
func New(session *atclient.APIClient, dir identity.Directory, hc *http.Client) (*Client, error) {
	c, err := NewDelegated(SessionDelegator{session}, dir, hc)
	if err != nil {
		return nil, err
	}
	c.Session = session
	return c, nil
}

// NewDelegated returns a client that only reads, with delegation tokens
// from d.
func NewDelegated(d Delegator, dir identity.Directory, hc *http.Client) (*Client, error) {
	key, err := atcrypto.GeneratePrivateKeyP256()
	if err != nil {
		return nil, err
	}
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{Delegator: d, Dir: dir, HTTP: hc, key: key, creds: map[string]credential{}}, nil
}

// DID is the account's DID, or empty for a client without a session.
func (c *Client) DID() syntax.DID {
	if c.Session == nil || c.Session.AccountDID == nil {
		return ""
	}
	return *c.Session.AccountDID
}

// SpaceHost resolves where a space authority serves its spaces: the DID
// document's #atproto_space_host service, else its PDS.
func (c *Client) SpaceHost(ctx context.Context, authority string) (string, error) {
	ident, err := c.lookup(ctx, authority)
	if err != nil {
		return "", err
	}
	if h := ident.GetServiceEndpoint("atproto_space_host"); h != "" {
		return h, nil
	}
	if h := ident.PDSEndpoint(); h != "" {
		return h, nil
	}
	return "", fmt.Errorf("%s publishes no space host or PDS", authority)
}

// PDSHost resolves an account's PDS.
func (c *Client) PDSHost(ctx context.Context, did string) (string, error) {
	ident, err := c.lookup(ctx, did)
	if err != nil {
		return "", err
	}
	if h := ident.PDSEndpoint(); h != "" {
		return h, nil
	}
	return "", fmt.Errorf("%s publishes no PDS", did)
}

func (c *Client) lookup(ctx context.Context, did string) (*identity.Identity, error) {
	d, err := syntax.ParseDID(did)
	if err != nil {
		return nil, err
	}
	return c.Dir.LookupDID(ctx, d)
}

// Credential returns a space credential for the space, minting a new one
// when none is cached or the cached one is close to expiry.
func (c *Client) Credential(ctx context.Context, spaceURI string) (string, error) {
	c.mu.Lock()
	cr, ok := c.creds[spaceURI]
	c.mu.Unlock()
	if ok && time.Until(cr.exp) > renewBefore {
		return cr.jwt, nil
	}
	return c.refreshCredential(ctx, spaceURI)
}

// Invalidate drops a cached credential, e.g. after a host rejects it.
func (c *Client) Invalidate(spaceURI string) {
	c.mu.Lock()
	delete(c.creds, spaceURI)
	c.mu.Unlock()
}

func (c *Client) refreshCredential(ctx context.Context, spaceURI string) (string, error) {
	ref, err := space.ParseRef(spaceURI)
	if err != nil {
		return "", err
	}
	token, err := c.Delegator.DelegationToken(ctx, spaceURI)
	if err != nil {
		return "", err
	}
	host, err := c.SpaceHost(ctx, ref.Authority)
	if err != nil {
		return "", err
	}
	// The exchange signs only the authorization; the signature's key becomes
	// the credential's bound key.
	headers, err := space.CreateSpaceSigHeaders(c.key, "Bearer "+token, "")
	if err != nil {
		return "", err
	}
	var out struct {
		Credential string `json:"credential"`
	}
	if err := c.do(ctx, http.MethodPost, host, "com.atproto.space.getSpaceCredential", nil, map[string]any{"space": spaceURI}, headers, &out); err != nil {
		return "", fmt.Errorf("getSpaceCredential: %w", err)
	}
	parsed, err := space.ParseSpaceToken(space.TokenCredential, out.Credential)
	if err != nil {
		return "", fmt.Errorf("getSpaceCredential returned a bad credential: %w", err)
	}
	cr := credential{jwt: out.Credential, exp: time.Unix(parsed.Payload.Exp, 0)}
	c.mu.Lock()
	c.creds[spaceURI] = cr
	c.mu.Unlock()
	return cr.jwt, nil
}

// SignedHeaders returns the headers presenting a space credential to an
// audience DID: the repo being read, the authority, or another service.
func (c *Client) SignedHeaders(ctx context.Context, spaceURI, audience string) (map[string]string, error) {
	cred, err := c.Credential(ctx, spaceURI)
	if err != nil {
		return nil, err
	}
	return space.CreateSpaceSigHeaders(c.key, "Atproto-Space "+cred, audience)
}

// Query makes a credentialed GET. On an auth failure it retries once with a
// fresh credential.
func (c *Client) Query(ctx context.Context, host, spaceURI, audience, nsid string, params url.Values, out any) error {
	return c.withCredential(ctx, spaceURI, func(h map[string]string) error {
		return c.do(ctx, http.MethodGet, host, nsid, params, nil, h, out)
	}, audience)
}

// Procedure makes a credentialed POST.
func (c *Client) Procedure(ctx context.Context, host, spaceURI, audience, nsid string, body, out any) error {
	return c.withCredential(ctx, spaceURI, func(h map[string]string) error {
		return c.do(ctx, http.MethodPost, host, nsid, nil, body, h, out)
	}, audience)
}

// QueryRaw makes a credentialed GET and returns the response body, for
// binary responses such as getRepo's CAR. The caller closes it.
func (c *Client) QueryRaw(ctx context.Context, host, spaceURI, audience, nsid string, params url.Values) (io.ReadCloser, error) {
	var body io.ReadCloser
	err := c.withCredential(ctx, spaceURI, func(h map[string]string) error {
		resp, err := c.send(ctx, http.MethodGet, host, nsid, params, nil, h)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			defer resp.Body.Close()
			return readError(resp)
		}
		body = resp.Body
		return nil
	}, audience)
	return body, err
}

func (c *Client) withCredential(ctx context.Context, spaceURI string, fn func(map[string]string) error, audience string) error {
	for attempt := 0; ; attempt++ {
		h, err := c.SignedHeaders(ctx, spaceURI, audience)
		if err != nil {
			return err
		}
		err = fn(h)
		var xe *Error
		if attempt == 0 && errors.As(err, &xe) && xe.Status == http.StatusUnauthorized {
			c.Invalidate(spaceURI)
			continue
		}
		return err
	}
}

func (c *Client) send(ctx context.Context, method, host, nsid string, params url.Values, body any, headers map[string]string) (*http.Response, error) {
	u := strings.TrimSuffix(host, "/") + "/xrpc/" + nsid
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return c.HTTP.Do(req)
}

func (c *Client) do(ctx context.Context, method, host, nsid string, params url.Values, body any, headers map[string]string, out any) error {
	resp, err := c.send(ctx, method, host, nsid, params, body, headers)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out)
}

func readError(resp *http.Response) error {
	xe := &Error{Status: resp.StatusCode}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(raw, xe) != nil || xe.Name == "" {
		xe.Name = http.StatusText(resp.StatusCode)
		xe.Message = strings.TrimSpace(string(raw))
	}
	return xe
}

// KeyResolver resolves the did:key a space token's issuer signs with, for
// space.VerifySpaceToken. A token names the account key (#atproto) or a
// dedicated space key (#atproto_space).
func KeyResolver(ctx context.Context, dir identity.Directory) space.SigningKeyFunc {
	return func(iss, kid string, fresh bool) (string, error) {
		id := strings.TrimPrefix(kid, "#")
		if id != "atproto" && id != "atproto_space" {
			return "", fmt.Errorf("unsupported space token kid %q", kid)
		}
		did, err := syntax.ParseDID(iss)
		if err != nil {
			return "", err
		}
		if fresh {
			_ = dir.Purge(ctx, did.AtIdentifier())
		}
		ident, err := dir.LookupDID(ctx, did)
		if err != nil {
			return "", err
		}
		pub, err := ident.GetPublicKey(id)
		if err != nil {
			return "", err
		}
		return pub.DIDKey(), nil
	}
}
