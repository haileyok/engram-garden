// Command engram stores and recalls memories in an Engram Garden memory
// space, as one agent's account. Run `engram login` once, then `engram
// remember` and `engram recall`. engram-mcp offers the same tools over MCP
// with the same settings.
package main

import (
	"context"
	"errors"
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

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"golang.org/x/term"

	"github.com/haileyok/engram-garden/internal/agent"
	"github.com/haileyok/engram-garden/internal/oauthfile"
	"github.com/haileyok/engram-garden/internal/spaceclient"
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
	idDir, err := agent.IdentitiesDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "engram:", err)
		os.Exit(1)
	}
	c := &cli{
		principalPath: filepath.Join(dir, "principal.json"),
		identitiesDir: idDir,
		principalSignIn: func(ctx context.Context, handle string, lines <-chan string) (agent.Account, []string, error) {
			// The scopes name the principal's PDS, which creates the agents.
			id, err := syntax.ParseAtIdentifier(handle)
			if err != nil {
				return agent.Account{}, nil, err
			}
			who, err := identity.DefaultDirectory().Lookup(ctx, id)
			if err != nil {
				return agent.Account{}, nil, fmt.Errorf("resolving %s: %w", handle, err)
			}
			pdsDID, err := agent.PDSDID(ctx, who.PDSEndpoint(), opts.HTTP)
			if err != nil {
				return agent.Account{}, nil, err
			}
			popts := opts
			popts.Scopes = agent.PrincipalScopes(pdsDID)
			acct, err := agent.LoginOAuth(ctx, handle, popts, agent.Prompt{Out: os.Stderr, Lines: lines, Browser: openBrowser})
			return acct, popts.Scopes, err
		},
		principalSession: func(ctx context.Context, p agent.Principal) (*atclient.APIClient, error) {
			if len(p.Scopes) == 0 {
				return nil, errors.New("the principal's sign-in predates agent creation: run `engram principal login` again")
			}
			popts := opts
			popts.Scopes = p.Scopes
			return agent.Settings{Account: p.Account}.Session(ctx, popts)
		},
		openLounge: func(ctx context.Context, s agent.Settings) (*agent.Lounge, agent.Identity, error) {
			return agent.OpenLounge(ctx, s, opts)
		},
		identityClient: func(ctx context.Context, s agent.Settings) (*spaceclient.Client, agent.Identity, error) {
			return agent.IdentityClient(ctx, s, opts)
		},
		spaceClient: func(sess *atclient.APIClient) (*spaceclient.Client, error) {
			return spaceclient.New(sess, identity.DefaultDirectory(), opts.HTTP)
		},
		verifyAgent: func(ctx context.Context, did string) (map[string]bool, error) {
			return agent.Principals(ctx, identity.DefaultDirectory(), opts.HTTP, did)
		},
		provisionHTTP: &http.Client{Timeout: time.Minute},
		spawnRefill: func() {
			self, err := os.Executable()
			if err != nil {
				return
			}
			cmd := exec.Command(self, "agent", "stock")
			cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
			cmd.SysProcAttr = detached()
			if err := cmd.Start(); err == nil {
				_ = cmd.Process.Release()
			}
		},
		in: os.Stdin, out: os.Stdout, err: os.Stderr, getenv: os.Getenv,
		configPath: filepath.Join(dir, "config.json"),
		open: func(ctx context.Context, s agent.Settings) (*agent.Spaces, error) {
			return agent.Open(ctx, s, opts)
		},
		signIn: func(ctx context.Context, handle, password string, lines <-chan string) (agent.Account, error) {
			if password == "" {
				return agent.LoginOAuth(ctx, handle, opts, agent.Prompt{Out: os.Stderr, Lines: lines, Browser: openBrowser})
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
		revoke:  func(ctx context.Context, a agent.Account) error { return agent.Revoke(ctx, a, opts) },
		pull:    ollamaPull(os.Stderr),
		browser: openBrowser,
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
