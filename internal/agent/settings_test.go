package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	spaceA = "at://did:plc:a/space/garden.engram.space/team"
	spaceB = "at://did:plc:b/space/garden.engram.space/team"
	spaceC = "at://did:plc:a/space/garden.engram.space/personal"
)

func TestSettingsSaveAndLoad(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "engram", "config.json")
	s, err := LoadSettings(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Spaces) != 0 || s.AppviewURL != DefaultAppviewURL || s.AppviewDID != "did:web:api.engram.garden" {
		t.Fatalf("defaults: %+v", s)
	}
	if _, err := s.UseSpace(spaceA); err != nil {
		t.Fatal(err)
	}
	s.Account = Account{Handle: "agent.test", DID: "did:plc:agent", SignIn: SignInOAuth, SessionID: "sid", Callback: "http://127.0.0.1:4000/callback", SignedInAt: time.Now().UTC().Truncate(time.Second)}
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config file mode: %v %v", info, err)
	}
	got, err := LoadSettings(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if def, ok := got.Default(); !ok || def.URI != spaceA || def.Name != "team" || got.Account != s.Account {
		t.Fatalf("round trip: %+v", got)
	}
}

// TestSettingsOneSpaceFile: a settings file from before several spaces, with
// a single "space", loads as that one space, the default.
func TestSettingsOneSpaceFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"space": "`+spaceA+`", "account": {"handle": "a.test"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if def, ok := s.Default(); !ok || def.URI != spaceA || len(s.Spaces) != 1 {
		t.Fatalf("old file: %+v", s)
	}
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, old := m["space"]; old || m["defaultSpace"] != "team" {
		t.Fatalf("saved in the new form: %s", raw)
	}
}

func TestSettingsSpaces(t *testing.T) {
	t.Parallel()
	var s Settings
	a, err := s.AddSpace(spaceA, "")
	if err != nil || a.Name != "team" {
		t.Fatalf("add: %+v %v", a, err)
	}
	// Another authority's space with the same key gets a different name.
	b, err := s.AddSpace(spaceB, "")
	if err != nil || b.Name == "team" || b.Name == "" {
		t.Fatalf("second team: %+v %v", b, err)
	}
	// Adding a space again keeps it once.
	if again, err := s.AddSpace(spaceA, ""); err != nil || again.Name != "team" || len(s.Spaces) != 2 {
		t.Fatalf("re-add: %+v %v %+v", again, err, s.Spaces)
	}
	if _, err := s.AddSpace(spaceC, "mine"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"all", "a/b", "at:x", " "} {
		if _, err := s.AddSpace("at://did:plc:z/space/garden.engram.space/x", bad); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	if _, err := s.AddSpace("not a uri", ""); err == nil {
		t.Error("bad URI accepted")
	}
	if _, err := s.AddSpace(spaceA, "mine"); err == nil {
		t.Error("a name in use for another space was accepted")
	}

	// The first space added is the default until another is chosen.
	if def, _ := s.Default(); def.URI != spaceA {
		t.Fatalf("default: %+v", def)
	}
	if _, err := s.UseSpace("mine"); err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{"": spaceC, "mine": spaceC, "team": spaceA, spaceB: spaceB} {
		got, err := s.Resolve(in)
		if err != nil || got.URI != want {
			t.Errorf("resolve %q: %+v %v", in, got, err)
		}
	}
	// A URI that isn't configured still resolves, for a one-off use.
	other := "at://did:plc:z/space/garden.engram.space/other"
	if got, err := s.Resolve(other); err != nil || got.URI != other || got.Name != "other" {
		t.Errorf("unconfigured URI: %+v %v", got, err)
	}
	if _, err := s.Resolve("nope"); err == nil {
		t.Error("unknown name resolved")
	}

	// Removing the default makes the first remaining one the default.
	if err := s.RemoveSpace("mine"); err != nil {
		t.Fatal(err)
	}
	if def, _ := s.Default(); def.URI != spaceA || len(s.Spaces) != 2 {
		t.Fatalf("after removing the default: %+v %+v", def, s.Spaces)
	}
	if err := s.RemoveSpace("mine"); err == nil {
		t.Error("removed a space twice")
	}
}

// TestSettingsEnv: ENGRAM_* variables win over the file, so engram-mcp
// setups that use them keep working.
func TestSettingsEnv(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	file := Settings{Account: Account{Handle: "file.test", SignIn: SignInOAuth, SessionID: "sid"}}
	if _, err := file.UseSpace(spaceC); err != nil {
		t.Fatal(err)
	}
	if err := file.Save(path); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"ENGRAM_SPACE":       spaceA,
		"ENGRAM_IDENTIFIER":  "env.test",
		"ENGRAM_PASSWORD":    "hunter2",
		"ENGRAM_APPVIEW_URL": "https://appview.example",
		"ENGRAM_EMBED_MODEL": "local-model",
	}
	s, err := LoadSettings(path, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	def, _ := s.Default()
	if def.URI != spaceA || s.Account.SignIn != SignInPassword || s.Account.Handle != "env.test" ||
		s.Account.Password != "hunter2" || s.AppviewURL != "https://appview.example" ||
		s.AppviewDID != "did:web:appview.example" || s.Embed.Model != "local-model" {
		t.Fatalf("env: %+v", s)
	}
	// The file's space is still there; ENGRAM_SPACE only chose the default.
	if _, err := s.Resolve("personal"); err != nil {
		t.Fatalf("file's space: %v", err)
	}
	if err := s.Check(); err != nil {
		t.Fatal(err)
	}
	if err := (Settings{}).Check(); err == nil {
		t.Fatal("empty settings passed the check")
	}

	// ENGRAM_SPACES lists the spaces outright; ENGRAM_SPACE may name one.
	env = map[string]string{"ENGRAM_SPACES": "work=" + spaceA + ", " + spaceC, "ENGRAM_SPACE": "personal"}
	s, err = LoadSettings(path, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	def, _ = s.Default()
	if len(s.Spaces) != 2 || s.Spaces[0].Name != "work" || def.URI != spaceC {
		t.Fatalf("ENGRAM_SPACES: %+v default %+v", s.Spaces, def)
	}
}

func TestSessionExpiry(t *testing.T) {
	t.Parallel()
	signed := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	a := Account{SignIn: SignInOAuth, SignedInAt: signed}
	if got := a.Expires(); !got.Equal(signed.Add(OAuthSessionLifetime)) {
		t.Fatalf("expires %v", got)
	}
	if (Account{SignIn: SignInPassword}).Expires() != (time.Time{}) {
		t.Fatal("password sign-ins don't expire")
	}
}
