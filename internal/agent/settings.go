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
	// Spaces are the memory spaces the agent uses, each with a short name.
	Spaces []SpaceEntry `json:"spaces,omitempty"`
	// DefaultSpace names the space used when none is given.
	DefaultSpace string `json:"defaultSpace,omitempty"`
	// Space is the one space of a settings file written before several
	// were possible. Loading moves it into Spaces.
	Space string `json:"space,omitempty"`
	// AppviewURL and AppviewDID locate the appview that indexes the spaces.
	AppviewURL string  `json:"appviewUrl,omitempty"`
	AppviewDID string  `json:"appviewDid,omitempty"`
	Account    Account `json:"account"`
	Embed      Embed   `json:"embed,omitzero"`
}

// SpaceEntry is a memory space the agent uses. Name is how commands and
// tools refer to it; it defaults to the space's key (the last part of its
// URI).
type SpaceEntry struct {
	Name string `json:"name"`
	URI  string `json:"uri"`
}

// AllSpaces is the space argument that recalls from every configured space.
// It can't name a space.
const AllSpaces = "all"

func checkSpaceName(name string) error {
	switch {
	case strings.TrimSpace(name) == "" || name != strings.TrimSpace(name):
		return errors.New("a space name can't be empty or start or end with spaces")
	case name == AllSpaces:
		return fmt.Errorf("%q is reserved: it means every space", AllSpaces)
	case strings.ContainsAny(name, "/:"):
		return fmt.Errorf("space name %q can't contain / or : (those are for URIs)", name)
	}
	return nil
}

// spaceKey is the default name for a space: its key.
func spaceKey(uri string) string {
	ref, err := space.ParseRef(uri)
	if err != nil {
		return ""
	}
	return ref.Skey
}

func (s *Settings) entry(nameOrURI string) (int, bool) {
	for i, e := range s.Spaces {
		if e.Name == nameOrURI || e.URI == nameOrURI {
			return i, true
		}
	}
	return -1, false
}

// AddSpace adds a space, named name (or, if empty, after its key), and
// returns its entry. A space already added keeps its entry.
func (s *Settings) AddSpace(uri, name string) (SpaceEntry, error) {
	if _, err := space.ParseRef(uri); err != nil {
		return SpaceEntry{}, fmt.Errorf("space %q: %w", uri, err)
	}
	if name != "" {
		if err := checkSpaceName(name); err != nil {
			return SpaceEntry{}, err
		}
	}
	for _, e := range s.Spaces {
		if e.URI == uri {
			if name != "" && name != e.Name {
				return SpaceEntry{}, fmt.Errorf("%s is already added, as %q", uri, e.Name)
			}
			return e, nil
		}
		if name != "" && e.Name == name {
			return SpaceEntry{}, fmt.Errorf("the name %q is already used for %s", name, e.URI)
		}
	}
	if name == "" {
		base := spaceKey(uri)
		if checkSpaceName(base) != nil {
			base = "space"
		}
		name = base
		for n := 2; ; n++ {
			if _, taken := s.entry(name); !taken {
				break
			}
			name = fmt.Sprintf("%s-%d", base, n)
		}
	}
	e := SpaceEntry{Name: name, URI: uri}
	s.Spaces = append(s.Spaces, e)
	if s.DefaultSpace == "" {
		s.DefaultSpace = name
	}
	return e, nil
}

// UseSpace makes a space the default, adding it first if it's a URI not
// yet added.
func (s *Settings) UseSpace(nameOrURI string) (SpaceEntry, error) {
	i, ok := s.entry(nameOrURI)
	if !ok {
		if !strings.HasPrefix(nameOrURI, "at://") {
			return SpaceEntry{}, fmt.Errorf("no space named %q: use its at:// URI to add it", nameOrURI)
		}
		e, err := s.AddSpace(nameOrURI, "")
		if err != nil {
			return SpaceEntry{}, err
		}
		s.DefaultSpace = e.Name
		return e, nil
	}
	s.DefaultSpace = s.Spaces[i].Name
	return s.Spaces[i], nil
}

// RemoveSpace stops using a space. If it was the default, the first
// remaining space becomes the default.
func (s *Settings) RemoveSpace(nameOrURI string) error {
	i, ok := s.entry(nameOrURI)
	if !ok {
		return fmt.Errorf("no space %q", nameOrURI)
	}
	removed := s.Spaces[i]
	s.Spaces = append(s.Spaces[:i:i], s.Spaces[i+1:]...)
	if s.DefaultSpace == removed.Name {
		s.DefaultSpace = ""
		if len(s.Spaces) > 0 {
			s.DefaultSpace = s.Spaces[0].Name
		}
	}
	return nil
}

// Default returns the default space, if any space is set up.
func (s Settings) Default() (SpaceEntry, bool) {
	if i, ok := s.entry(s.DefaultSpace); ok {
		return s.Spaces[i], true
	}
	if len(s.Spaces) > 0 {
		return s.Spaces[0], true
	}
	return SpaceEntry{}, false
}

// Resolve finds a space by name or URI; "" is the default. A URI that isn't
// set up resolves too, named after its key, for one-off use.
func (s Settings) Resolve(nameOrURI string) (SpaceEntry, error) {
	if nameOrURI == "" {
		if e, ok := s.Default(); ok {
			return e, nil
		}
		return SpaceEntry{}, errors.New("no memory space set up: run `engram init`, or `engram use <space URI>`")
	}
	if i, ok := s.entry(nameOrURI); ok {
		return s.Spaces[i], nil
	}
	if strings.HasPrefix(nameOrURI, "at://") {
		if _, err := space.ParseRef(nameOrURI); err != nil {
			return SpaceEntry{}, fmt.Errorf("space %q: %w", nameOrURI, err)
		}
		return SpaceEntry{Name: spaceKey(nameOrURI), URI: nameOrURI}, nil
	}
	return SpaceEntry{}, fmt.Errorf("no space named %q (see `engram spaces`)", nameOrURI)
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
	// A file from before several spaces names one space.
	if s.Space != "" {
		if _, err := s.UseSpace(s.Space); err != nil {
			return s, fmt.Errorf("%s: %w", path, err)
		}
		s.Space = ""
	}
	if getenv != nil {
		if err := s.applyEnv(getenv); err != nil {
			return s, err
		}
	}
	s.defaults()
	return s, nil
}

func (s *Settings) applyEnv(getenv func(string) string) error {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }
	set := func(dst *string, k string) {
		if v := get(k); v != "" {
			*dst = v
		}
	}
	// ENGRAM_SPACES replaces the spaces: comma-separated URIs, each
	// optionally name=URI.
	if v := get("ENGRAM_SPACES"); v != "" {
		s.Spaces, s.DefaultSpace = nil, ""
		for _, item := range strings.Split(v, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			name, uri, named := strings.Cut(item, "=")
			if !named {
				name, uri = "", item
			}
			if _, err := s.AddSpace(strings.TrimSpace(uri), strings.TrimSpace(name)); err != nil {
				return fmt.Errorf("ENGRAM_SPACES: %w", err)
			}
		}
	}
	// ENGRAM_SPACE chooses the default, by name or URI (added if new).
	if v := get("ENGRAM_SPACE"); v != "" {
		if _, err := s.UseSpace(v); err != nil {
			return fmt.Errorf("ENGRAM_SPACE: %w", err)
		}
	}
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
	return nil
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
	if len(s.Spaces) == 0 {
		return errors.New("no memory space set up: run `engram init`, or `engram use <space URI>`, or set ENGRAM_SPACE")
	}
	for _, e := range s.Spaces {
		if _, err := space.ParseRef(e.URI); err != nil {
			return fmt.Errorf("space %q: %w", e.Name, err)
		}
	}
	return s.CheckAccount()
}

// CheckAccount reports what's missing to sign in and reach the appview,
// whatever the spaces.
func (s Settings) CheckAccount() error {
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
