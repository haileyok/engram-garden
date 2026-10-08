package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/haileyok/cocoon/space"
)

// DefaultAppviewURL is the Engram Garden appview.
const DefaultAppviewURL = "https://api.engram.garden"

// How an agent signs in to its account.
const (
	// SignInOAuth keeps an OAuth session: no password is stored, but the
	// account's server ends it after OAuthSessionLifetime.
	SignInOAuth = "oauth"
	// SignInPassword stores the account's password.
	SignInPassword = "password"
)

// OAuthSessionLifetime is how long an OAuth sign-in lasts. A command-line
// tool is a public OAuth client, and servers limit those sessions; Cocoon
// ends them two weeks after sign-in, however often they're used.
const OAuthSessionLifetime = 14 * 24 * time.Hour

// Settings configure an agent: the file `engram init` writes, with ENGRAM_*
// environment variables taking priority.
type Settings struct {
	// Space is the memory space's URI.
	Space string `json:"space"`
	// AppviewURL and AppviewDID locate the appview that indexes the space.
	AppviewURL string  `json:"appviewUrl,omitempty"`
	AppviewDID string  `json:"appviewDid,omitempty"`
	Account    Account `json:"account"`
	Embed      Embed   `json:"embed,omitzero"`
}

// Account is the agent's own ATProto account and how it signs in.
type Account struct {
	Handle string `json:"handle,omitempty"`
	DID    string `json:"did,omitempty"`
	// SignIn is SignInOAuth or SignInPassword.
	SignIn   string `json:"signIn,omitempty"`
	Password string `json:"password,omitempty"`
	// PDSHost skips resolving the account's PDS (password sign-in).
	PDSHost string `json:"pdsHost,omitempty"`
	// SessionID names the OAuth session, and Callback the loopback address
	// its client was registered with (refreshing needs the same one).
	SessionID  string    `json:"sessionId,omitempty"`
	Callback   string    `json:"callback,omitempty"`
	SignedInAt time.Time `json:"signedInAt,omitzero"`
}

// Expires is when an OAuth sign-in ends, or zero for a password.
func (a Account) Expires() time.Time {
	if a.SignIn != SignInOAuth || a.SignedInAt.IsZero() {
		return time.Time{}
	}
	return a.SignedInAt.Add(OAuthSessionLifetime)
}

// Embed configures the embedding endpoint.
type Embed struct {
	// URL is an OpenAI-compatible base URL (default Ollama's).
	URL    string `json:"url,omitempty"`
	APIKey string `json:"apiKey,omitempty"`
	// Model is the local model's name, when it differs from the space's.
	Model string `json:"model,omitempty"`
	// Digest is the local model's digest, for endpoints that aren't Ollama.
	Digest string `json:"digest,omitempty"`
	// Provider is "openai" (default) or "hashing" (offline, for tests).
	Provider string `json:"provider,omitempty"`
}

// ConfigDir is where the CLI keeps its settings and OAuth sessions:
// $ENGRAM_CONFIG_DIR, else the user's config directory's "engram".
func ConfigDir() (string, error) {
	if d := os.Getenv("ENGRAM_CONFIG_DIR"); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "engram"), nil
}

// ConfigPath is the settings file in ConfigDir.
func ConfigPath() (string, error) {
	d, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.json"), nil
}

// LoadSettings reads the settings file (a missing one is empty) and applies
// ENGRAM_* variables from getenv (nil for none).
func LoadSettings(path string, getenv func(string) string) (Settings, error) {
	var s Settings
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return s, err
	default:
		if err := json.Unmarshal(raw, &s); err != nil {
			return s, fmt.Errorf("%s: %w", path, err)
		}
	}
	if getenv != nil {
		s.applyEnv(getenv)
	}
	s.defaults()
	return s, nil
}

func (s *Settings) applyEnv(getenv func(string) string) {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }
	set := func(dst *string, k string) {
		if v := get(k); v != "" {
			*dst = v
		}
	}
	set(&s.Space, "ENGRAM_SPACE")
	if v := get("ENGRAM_APPVIEW_URL"); v != "" && v != s.AppviewURL {
		// A different appview has its own DID.
		s.AppviewURL, s.AppviewDID = v, ""
	}
	set(&s.AppviewDID, "ENGRAM_APPVIEW_DID")
	if ident, pass := get("ENGRAM_IDENTIFIER"), get("ENGRAM_PASSWORD"); ident != "" && pass != "" {
		s.Account = Account{Handle: ident, SignIn: SignInPassword, Password: pass, PDSHost: get("ENGRAM_PDS_HOST")}
	}
	set(&s.Embed.URL, "ENGRAM_EMBED_URL")
	set(&s.Embed.APIKey, "ENGRAM_EMBED_API_KEY")
	set(&s.Embed.Model, "ENGRAM_EMBED_MODEL")
	set(&s.Embed.Digest, "ENGRAM_EMBED_MODEL_DIGEST")
	set(&s.Embed.Provider, "ENGRAM_EMBED_PROVIDER")
}

// SetAppview points the settings at another appview, whose DID follows from
// its host.
func (s *Settings) SetAppview(appviewURL string) {
	s.AppviewURL, s.AppviewDID = appviewURL, ""
	s.defaults()
}

func (s *Settings) defaults() {
	if s.AppviewURL == "" {
		s.AppviewURL = DefaultAppviewURL
	}
	s.AppviewURL = strings.TrimSuffix(s.AppviewURL, "/")
	if s.AppviewDID == "" {
		// A did:web appview's DID follows from its host.
		if u, err := url.Parse(s.AppviewURL); err == nil && u.Hostname() != "" {
			s.AppviewDID = "did:web:" + u.Hostname()
		}
	}
}

// Check reports what's missing before the agent can run.
func (s Settings) Check() error {
	if s.Space == "" {
		return errors.New("no memory space set up: run `engram init`, or set ENGRAM_SPACE")
	}
	if _, err := space.ParseRef(s.Space); err != nil {
		return fmt.Errorf("space %q: %w", s.Space, err)
	}
	if u, err := url.Parse(s.AppviewURL); err != nil || u.Host == "" {
		return fmt.Errorf("appview URL %q isn't a URL", s.AppviewURL)
	}
	switch s.Account.SignIn {
	case SignInPassword:
		if s.Account.Handle == "" || s.Account.Password == "" {
			return errors.New("password sign-in needs a handle and a password")
		}
	case SignInOAuth:
		if s.Account.DID == "" || s.Account.SessionID == "" || s.Account.Callback == "" {
			return errors.New("not signed in: run `engram login`")
		}
	default:
		return errors.New("not signed in: run `engram init` (or set ENGRAM_IDENTIFIER and ENGRAM_PASSWORD)")
	}
	return nil
}

// Save writes the settings file, readable only by its owner.
func (s Settings) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
