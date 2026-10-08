package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSettingsSaveAndLoad(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "engram", "config.json")
	s, err := LoadSettings(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Space != "" || s.AppviewURL != DefaultAppviewURL || s.AppviewDID != "did:web:api.engram.garden" {
		t.Fatalf("defaults: %+v", s)
	}
	s.Space = "at://did:plc:a/space/garden.engram.space/m"
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
	if got.Space != s.Space || got.Account != s.Account {
		t.Fatalf("round trip: %+v", got)
	}
}

// TestSettingsEnv: ENGRAM_* variables win over the file, so engram-mcp
// setups that use them keep working.
func TestSettingsEnv(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	file := Settings{Space: "at://did:plc:a/space/garden.engram.space/file", Account: Account{Handle: "file.test", SignIn: SignInOAuth, SessionID: "sid"}}
	if err := file.Save(path); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"ENGRAM_SPACE":       "at://did:plc:a/space/garden.engram.space/env",
		"ENGRAM_IDENTIFIER":  "env.test",
		"ENGRAM_PASSWORD":    "hunter2",
		"ENGRAM_APPVIEW_URL": "https://appview.example",
		"ENGRAM_EMBED_MODEL": "local-model",
	}
	s, err := LoadSettings(path, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if s.Space != env["ENGRAM_SPACE"] || s.Account.SignIn != SignInPassword || s.Account.Handle != "env.test" ||
		s.Account.Password != "hunter2" || s.AppviewURL != "https://appview.example" ||
		s.AppviewDID != "did:web:appview.example" || s.Embed.Model != "local-model" {
		t.Fatalf("env: %+v", s)
	}
	if err := s.Check(); err != nil {
		t.Fatal(err)
	}
	if err := (Settings{}).Check(); err == nil {
		t.Fatal("empty settings passed the check")
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
