package agent

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// An account that signs in with a key has no password and no browser
// sign-in: its runtime holds a private key, registered with the account's
// PDS as a sign-in key, and exchanges a short JWT signed with it for an
// ordinary OAuth session (the JWT bearer grant, RFC 7523). See
// docs/design/agent-spawning.md.

// SignInKey signs in with a key file (Account.KeyFile).
const SignInKey = "key"

// JWTBearerGrant is the OAuth grant type for signing in with a key.
const JWTBearerGrant = "urn:ietf:params:oauth:grant-type:jwt-bearer"

// keyCallback is the loopback address in a key sign-in's client ID. Nothing
// listens there: the grant has no redirect. It only has to stay the same,
// since refreshing needs the client the session was issued to.
const keyCallback = "http://127.0.0.1/callback"

// NewSignInKey generates a sign-in key.
func NewSignInKey() (*atcrypto.PrivateKeyK256, error) { return atcrypto.GeneratePrivateKeyK256() }

// SaveSignInKey writes a sign-in key to path, readable only by its owner.
// It never replaces a file that's there.
func SaveSignInKey(path string, key atcrypto.PrivateKeyExportable) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(f, key.Multibase()+"\n"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// LoadSignInKey reads a sign-in key file.
func LoadSignInKey(path string) (atcrypto.PrivateKey, error) {
	if err := checkKeyFile(path); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return atcrypto.ParsePrivateMultibase(strings.TrimSpace(string(b)))
}

// checkKeyFile refuses a key file others can read.
func checkKeyFile(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("sign-in key: %w", err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("sign-in key %s can be read by others (mode %v): chmod 600 it", path, st.Mode().Perm())
	}
	return nil
}

// checkServerURL refuses a server reached over plain HTTP, unless it's on
// this machine.
func checkServerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%q isn't a server URL", raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		h := u.Hostname()
		if h == "localhost" || net.ParseIP(h).IsLoopback() {
			return nil
		}
	}
	return fmt.Errorf("%s must use https", raw)
}

// authServer is what signing in needs from an account's authorization
// server.
type authServer struct {
	Issuer             string `json:"issuer"`
	TokenEndpoint      string `json:"token_endpoint"`
	RevocationEndpoint string `json:"revocation_endpoint"`
}

func getJSON(ctx context.Context, hc *http.Client, u string, out any) error {
	if err := checkServerURL(u); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", u, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// discover finds the account's PDS and its authorization server.
func discover(ctx context.Context, a Account, o Options, hc *http.Client) (string, authServer, error) {
	var as authServer
	host := a.PDSHost
	if host == "" {
		did, err := syntax.ParseDID(a.DID)
		if err != nil {
			return "", as, fmt.Errorf("account DID %q: %w", a.DID, err)
		}
		id, err := o.dir().LookupDID(ctx, did)
		if err != nil {
			return "", as, err
		}
		if host = id.PDSEndpoint(); host == "" {
			return "", as, fmt.Errorf("%s has no PDS", a.DID)
		}
	}
	host = strings.TrimRight(host, "/")
	var pr struct {
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := getJSON(ctx, hc, host+"/.well-known/oauth-protected-resource", &pr); err != nil {
		return "", as, fmt.Errorf("finding the authorization server: %w", err)
	}
	if len(pr.AuthorizationServers) == 0 {
		return "", as, fmt.Errorf("%s names no authorization server", host)
	}
	issuer := strings.TrimRight(pr.AuthorizationServers[0], "/")
	if err := getJSON(ctx, hc, issuer+"/.well-known/oauth-authorization-server", &as); err != nil {
		return "", as, fmt.Errorf("reading the authorization server's metadata: %w", err)
	}
	if strings.TrimRight(as.Issuer, "/") != issuer || as.TokenEndpoint == "" {
		return "", as, fmt.Errorf("authorization server %s has bad metadata (issuer %q)", issuer, as.Issuer)
	}
	if err := checkServerURL(as.TokenEndpoint); err != nil {
		return "", as, err
	}
	return host, as, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// assertion is the signed JWT a key sign-in presents.
func assertion(key atcrypto.PrivateKey, did, aud string, now time.Time) (string, error) {
	alg := ""
	switch key.(type) {
	case *atcrypto.PrivateKeyK256:
		alg = "ES256K"
	case *atcrypto.PrivateKeyP256:
		alg = "ES256"
	default:
		return "", errors.New("unsupported sign-in key type")
	}
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return "", err
	}
	h, _ := json.Marshal(map[string]string{"alg": alg, "typ": "JWT"})
	c, _ := json.Marshal(map[string]any{
		"iss": did, "sub": did, "aud": aud, "jti": hex.EncodeToString(jti),
		"iat": now.Unix(), "exp": now.Add(2 * time.Minute).Unix(),
	})
	input := b64(h) + "." + b64(c)
	sig, err := key.HashAndSign([]byte(input))
	if err != nil {
		return "", err
	}
	return input + "." + b64(sig), nil
}

// loginKey signs a key account in for scopes and saves the session as
// sessionID.
func loginKey(ctx context.Context, a Account, scopes []string, sessionID string, o Options) (*oauth.ClientSessionData, error) {
	if o.Store == nil {
		return nil, errors.New("no OAuth session store")
	}
	key, err := LoadSignInKey(a.KeyFile)
	if err != nil {
		return nil, err
	}
	did, err := syntax.ParseDID(a.DID)
	if err != nil {
		return nil, fmt.Errorf("account DID %q: %w", a.DID, err)
	}
	hc := o.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: time.Minute}
	}
	host, as, err := discover(ctx, a, o, hc)
	if err != nil {
		return nil, err
	}
	dpopKey, err := atcrypto.GeneratePrivateKeyP256()
	if err != nil {
		return nil, err
	}
	clientID := oauthApp(keyCallback, scopes, o).Config.ClientID
	nonce := ""
	var tok oauth.TokenResponse
	for attempt := 0; ; attempt++ {
		jwt, err := assertion(key, did.String(), as.Issuer, time.Now())
		if err != nil {
			return nil, err
		}
		form := url.Values{"grant_type": {JWTBearerGrant}, "client_id": {clientID}, "assertion": {jwt}, "scope": {strings.Join(scopes, " ")}}
		proof, err := oauth.NewAuthDPoP(http.MethodPost, as.TokenEndpoint, nonce, dpopKey)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, as.TokenEndpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("DPoP", proof)
		resp, err := hc.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if n := resp.Header.Get("DPoP-Nonce"); n != "" {
			nonce = n
		}
		if resp.StatusCode == http.StatusOK {
			if err := json.Unmarshal(body, &tok); err != nil {
				return nil, fmt.Errorf("signing in with the key: %w", err)
			}
			break
		}
		var e struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Error == "use_dpop_nonce" && attempt == 0 {
			continue
		}
		msg := e.Error
		if e.Description != "" {
			msg += ": " + e.Description
		}
		if msg == "" {
			msg = resp.Status
		}
		return nil, fmt.Errorf("signing in with the key was refused (%s)", msg)
	}
	if tok.Subject != did.String() || tok.AccessToken == "" {
		return nil, fmt.Errorf("the server signed in %q, not %s", tok.Subject, did)
	}
	sess := oauth.ClientSessionData{
		AccountDID: did, SessionID: sessionID, HostURL: host,
		AuthServerURL: as.Issuer, AuthServerTokenEndpoint: as.TokenEndpoint, AuthServerRevocationEndpoint: as.RevocationEndpoint,
		Scopes: strings.Fields(tok.Scope), AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken,
		DPoPAuthServerNonce: nonce, DPoPPrivateKeyMultibase: dpopKey.Multibase(),
	}
	if err := o.Store.SaveSession(ctx, sess); err != nil {
		return nil, err
	}
	return &sess, nil
}

// NewAccount is an account to create that signs in with a key.
type NewAccount struct {
	Handle string `json:"handle"`
	// Email is required by most servers; agents use a placeholder.
	Email      string `json:"email,omitempty"`
	InviteCode string `json:"inviteCode,omitempty"`
	// RecoveryKey is put first in the account's PLC rotation keys.
	RecoveryKey string `json:"recoveryKey,omitempty"`
	// AuthKey is the sign-in key's did:key.
	AuthKey string `json:"authKey"`
}

// CreateKeyAccount creates an account with no password, signing in with
// its key (com.atproto.server.createAccount with authKey), on the PDS at
// host. It returns the new account's DID.
func CreateKeyAccount(ctx context.Context, hc *http.Client, host string, in NewAccount) (string, error) {
	if err := checkServerURL(host); err != nil {
		return "", err
	}
	if hc == nil {
		hc = &http.Client{Timeout: time.Minute}
	}
	body, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(host, "/")+"/xrpc/com.atproto.server.createAccount", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		DID     string `json:"did"`
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	if resp.StatusCode != http.StatusOK {
		msg := out.Error
		if out.Message != "" {
			msg += ": " + out.Message
		}
		if msg == "" {
			msg = resp.Status
		}
		return "", fmt.Errorf("creating %s: %s", in.Handle, msg)
	}
	if _, err := syntax.ParseDID(out.DID); err != nil {
		return "", fmt.Errorf("creating %s: the server returned DID %q", in.Handle, out.DID)
	}
	return out.DID, nil
}

// keySessionID names a key account's own session: the same every run, so a
// later run resumes it.
func keySessionID(a Account) string { return "key-" + hashName("key", a.DID, a.KeyFile)[:32] }

// keySession opens a key account's session: the saved one, or a new
// sign-in when there's none.
func keySession(ctx context.Context, a Account, o Options) (*atclient.APIClient, error) {
	did, err := syntax.ParseDID(a.DID)
	if err != nil {
		return nil, fmt.Errorf("account DID %q: %w", a.DID, err)
	}
	sid := keySessionID(a)
	lock := lockPath(o.LockDir, did, sid)
	app := oauthApp(keyCallback, Scopes, o)
	if err := func() error {
		if lock != "" {
			unlock, err := lockFile(lock)
			if err != nil {
				return err
			}
			defer unlock()
		}
		if _, err := o.Store.GetSession(ctx, did, sid); err == nil {
			return nil
		}
		_, err := loginKey(ctx, a, Scopes, sid, o)
		return err
	}(); err != nil {
		return nil, err
	}
	sess, err := app.ResumeSession(ctx, did, sid)
	if err != nil {
		return nil, err
	}
	api := sess.APIClient()
	api.Auth = &sharedSession{app: app, did: did, sessionID: sid, lock: lock}
	return api, nil
}

// KeySignInFor signs a key account in for one task, asking only for
// scopes, as a session of its own: end revokes it. It returns the scopes
// granted, which the caller checks.
func KeySignInFor(ctx context.Context, a Account, scopes []string, o Options) (*atclient.APIClient, []string, func(), error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return nil, nil, nil, err
	}
	data, err := loginKey(ctx, a, scopes, "once-"+hex.EncodeToString(raw), o)
	if err != nil {
		return nil, nil, nil, err
	}
	app := oauthApp(keyCallback, scopes, o)
	end := func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = app.Logout(ctx, data.AccountDID, data.SessionID)
	}
	sess, err := app.ResumeSession(ctx, data.AccountDID, data.SessionID)
	if err != nil {
		end()
		return nil, nil, nil, err
	}
	return sess.APIClient(), data.Scopes, end, nil
}
