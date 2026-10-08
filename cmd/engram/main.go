// Command engram stores and recalls memories in an Engram Garden memory
// space, as one agent's account. Run `engram init` once, then `engram
// remember` and `engram recall`. engram-mcp offers the same tools over MCP
// with the same settings.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/haileyok/engram-garden/internal/agent"
	"github.com/haileyok/engram-garden/internal/oauthfile"
)

func main() {
	// Libraries log routine retries (an authorization server asking for a
	// DPoP nonce, say) as warnings; at a terminal only errors are news.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dir, err := agent.ConfigDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "engram:", err)
		os.Exit(1)
	}
	store := &oauthfile.FileStore{Dir: filepath.Join(dir, "oauth")}
	store.Sweep() // sign-ins nobody finished
	opts := agent.Options{Store: store, LockDir: store.Dir, HTTP: &http.Client{Timeout: time.Minute}}
	c := &cli{
		in: os.Stdin, out: os.Stdout, err: os.Stderr, getenv: os.Getenv,
		configPath: filepath.Join(dir, "config.json"),
		open: func(ctx context.Context, s agent.Settings) (*agent.Agent, error) {
			return agent.Open(ctx, s, opts)
		},
		signIn: func(ctx context.Context, handle, password string) (agent.Account, error) {
			if password == "" {
				return agent.LoginOAuth(ctx, handle, opts, agent.Prompt{Out: os.Stderr, In: os.Stdin, Browser: openBrowser})
			}
			acct := agent.Account{Handle: handle, SignIn: agent.SignInPassword, Password: password}
			sess, err := agent.Settings{Account: acct}.Session(ctx, opts)
			if err != nil {
				return agent.Account{}, fmt.Errorf("signing in as %s: %w", handle, err)
			}
			if sess.AccountDID != nil {
				acct.DID = sess.AccountDID.String()
			}
			acct.SignedInAt = time.Now().UTC()
			return acct, nil
		},
		revoke: func(ctx context.Context, a agent.Account) error { return agent.Revoke(ctx, a, opts) },
		pull:   ollamaPull(os.Stderr),
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		c.readSecret = func(prompt string) (string, error) {
			fmt.Fprint(os.Stderr, prompt)
			defer fmt.Fprintln(os.Stderr)
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			return string(b), err
		}
	}
	os.Exit(c.run(ctx, os.Args[1:]))
}

// openBrowser opens a URL in the desktop's browser, if there is one.
func openBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return fmt.Errorf("no display")
		}
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}
