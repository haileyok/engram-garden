package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/haileyok/engram-garden/internal/agent"
	"github.com/haileyok/engram-garden/internal/embed"
)

// cli runs one command. Its fields are the outside world, so tests can
// replace them.
type cli struct {
	in       io.Reader
	out, err io.Writer
	getenv   func(string) string
	// configPath is the settings file.
	configPath string
	// open signs in with settings.
	open func(context.Context, agent.Settings) (*agent.Spaces, error)
	// signIn signs in to an account: OAuth, or password when password is set.
	// lines is input, which an OAuth sign-in watches for an address pasted
	// from the browser.
	signIn func(ctx context.Context, handle, password string, lines <-chan string) (agent.Account, error)
	// readSecret reads a password without echoing it, from a terminal; nil
	// when stdin isn't one, so the password is read as a line.
	readSecret func(prompt string) (string, error)
	// revoke ends an earlier OAuth sign-in after signing in again.
	revoke func(ctx context.Context, a agent.Account) error
	// pull fetches an embedding model (`ollama pull`); nil when unavailable.
	pull func(ctx context.Context, model string) error
	// browser opens a URL; nil, or a failure, leaves it to the person.
	browser func(string) error
	// wait is how often engram index checks the appview (default 2s).
	wait time.Duration

	json bool
	buf  *bufio.Reader
	// lines, once an OAuth sign-in starts, is the only reader of buf: a
	// goroutine hands over each line as it's wanted. The sign-in can then
	// wait for a pasted address and the browser at once, and a line it
	// doesn't take is left for the next prompt.
	lines chan string
}

// reader is stdin, buffered once so prompts and stdin text share it. Don't
// use it after lineInput.
func (c *cli) reader() *bufio.Reader {
	if c.buf == nil {
		c.buf = bufio.NewReader(c.in)
	}
	return c.buf
}

// lineInput hands over input line by line, from then on.
func (c *cli) lineInput() <-chan string {
	if c.lines == nil {
		c.lines = make(chan string)
		go func() {
			defer close(c.lines)
			for {
				line, err := c.readBuffered()
				if err != nil {
					return
				}
				c.lines <- line
			}
		}()
	}
	return c.lines
}

// readLine reads one line of input, whole.
func (c *cli) readLine() (string, error) {
	if c.lines != nil {
		line, ok := <-c.lines
		if !ok {
			return "", io.EOF
		}
		return line, nil
	}
	return c.readBuffered()
}

func (c *cli) readBuffered() (string, error) {
	line, err := c.reader().ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		return line, err
	}
	return line, nil
}

// reportedError is a failure the command already reported in its output.
type reportedError struct{ error }

const usage = `engram: shared memory for agents, on Engram Garden

Account:
  engram login [--handle h] [--password]   sign in as the agent's account and check its spaces (OAuth sign-ins last two weeks)
  engram logout                            end the sign-in and forget the account

Spaces:
  engram spaces                       the spaces you use, each one's embedding model, and others you belong to
  engram use <name|URI>               make a space the default (a new URI is added first)
  engram spaces add <URI> [--name n]  use another space
  engram spaces remove <name>         stop using a space

Spaces you run (--space picks one; otherwise the default):
  engram create <name> [--model m]           make a new space (and declare its embedding model)
  engram members                             who's in it
  engram members add <handle> [--read-only]  add an account; members remove <handle> takes one out
  engram model [--set m | --next m | --promote | --cancel]   show or change its embedding model
  engram index [--stop]                      let the appview index it (approve in the browser)

Use (--space <name|URI> picks a space; otherwise the default):
  engram remember <text> [-t tag]... [--source s]   store a memory (text from stdin if omitted)
  engram recall <query> [-n 10] [-t tag]...          search by meaning, in every space unless --space
  engram list [-n 25] [--author did] [-t tag]...     newest memories first
  engram get <uri>                                   one memory
  engram forget <uri>                                delete one of your own memories
  engram status                                      account, spaces, models and sign-in

Add --json to any command for machine-readable output. MCP clients can run
engram-mcp, which uses the same settings.

Settings: %s
`

func (c *cli) run(ctx context.Context, args []string) int {
	// --json and --config may appear anywhere.
	var rest []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--":
			// Everything after -- is the command's, unread.
			rest = append(rest, args[i:]...)
			i = len(args)
		case a == "--json" || a == "-json":
			c.json = true
		case a == "--config":
			if i+1 >= len(args) {
				fmt.Fprintln(c.err, "engram: --config needs a path")
				return 2
			}
			c.configPath = args[i+1]
			i++
		case strings.HasPrefix(a, "--config="):
			c.configPath = strings.TrimPrefix(a, "--config=")
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 || rest[0] == "help" || rest[0] == "-h" || rest[0] == "--help" {
		fmt.Fprintf(c.out, usage, c.configPath)
		return 0
	}
	cmds := map[string]func(context.Context, []string) error{
		"init": c.cmdLogin, "login": c.cmdLogin, "logout": c.cmdLogout, "remember": c.cmdRemember, "recall": c.cmdRecall,
		"list": c.cmdList, "get": c.cmdGet, "forget": c.cmdForget, "status": c.cmdStatus,
		"spaces": c.cmdSpaces, "use": c.cmdUse,
		"create": c.cmdCreate, "members": c.cmdMembers, "model": c.cmdModel, "index": c.cmdIndex,
	}
	cmd, ok := cmds[rest[0]]
	if !ok {
		fmt.Fprintf(c.err, "engram: unknown command %q (try engram help)\n", rest[0])
		return 2
	}
	if err := cmd(ctx, rest[1:]); err != nil {
		var ue usageError
		if errors.As(err, &ue) {
			fmt.Fprintf(c.err, "engram %s: %v\n", rest[0], err)
			return 2
		}
		var re reportedError
		if errors.As(err, &re) {
			// The output already says what's wrong (and with --json, is
			// the one JSON document).
			if !c.json {
				fmt.Fprintf(c.err, "engram %s: %v\n", rest[0], err)
			}
			return 1
		}
		err = agent.Explain(err)
		if c.json {
			_ = json.NewEncoder(c.out).Encode(map[string]string{"error": err.Error()})
		} else {
			fmt.Fprintf(c.err, "engram %s: %v\n", rest[0], err)
		}
		return 1
	}
	return 0
}

type usageError struct{ error }

// strings is a repeatable string flag.
type stringsFlag []string

func (s *stringsFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringsFlag) Set(v string) error { *s = append(*s, v); return nil }

// parse parses flags that may come before, between or after positional
// arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageError{err}
		}
		rest := fs.Args()
		// The flag package stops at, and consumes, "--": the rest is text.
		if used := len(args) - len(rest); used > 0 && args[used-1] == "--" {
			return append(pos, rest...), nil
		}
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func (c *cli) settings() (agent.Settings, error) {
	return agent.LoadSettings(c.configPath, c.getenv)
}

// fileSettings reads only the settings file, for commands that change and
// save it: ENGRAM_* variables apply to a run, not to the file.
func (c *cli) fileSettings() (agent.Settings, error) {
	return agent.LoadSettings(c.configPath, nil)
}

func (c *cli) agent(ctx context.Context) (*agent.Spaces, agent.Settings, error) {
	s, err := c.settings()
	if err != nil {
		return nil, s, err
	}
	if err := s.Check(); err != nil {
		return nil, s, err
	}
	a, err := c.open(ctx, s)
	return a, s, err
}

// print writes v as JSON with --json, else human text.
func (c *cli) print(v any, human func(io.Writer)) {
	if c.json {
		e := json.NewEncoder(c.out)
		e.SetIndent("", "  ")
		_ = e.Encode(v)
		return
	}
	human(c.out)
}

// ---- setup ----

// cmdLogin signs in to the agent's account (`engram init` is the same
// command), then checks the spaces set up, if any.
func (c *cli) cmdLogin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	spaceURI := fs.String("space", "", "also use this space (by URI), as the default")
	handle := fs.String("handle", "", "the agent's account handle (default: the one signed in before)")
	password := fs.Bool("password", false, "sign in with the account's password instead of OAuth (for machines without a browser)")
	appview := fs.String("appview", "", "the appview's URL (default "+agent.DefaultAppviewURL+")")
	embedURL := fs.String("embed-url", "", "OpenAI-compatible embedding endpoint (default Ollama at http://localhost:11434/v1)")
	yes := fs.Bool("yes", false, "answer yes to questions (pulling a space's model)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	s, err := c.fileSettings()
	if err != nil {
		return err
	}
	if *appview != "" {
		s.SetAppview(*appview)
	}
	if *embedURL != "" {
		s.Embed.URL = *embedURL
	}
	checkSpace := ""
	if *spaceURI != "" {
		e, err := s.UseSpace(*spaceURI)
		if err != nil {
			return usageError{err}
		}
		checkSpace = e.Name
	}
	h := *handle
	if h == "" {
		h = s.Account.Handle
	}
	if h == "" {
		if h, err = c.ask("The agent's account handle: "); err != nil {
			return err
		}
	}
	// Keep the way the account signed in last time unless told otherwise.
	usePassword := *password || (s.Account.SignIn == agent.SignInPassword && !flagSet(fs, "password") && h == s.Account.Handle)
	acct, err := c.doSignIn(ctx, h, usePassword)
	if err != nil {
		return err
	}
	if _, err = c.saveAccount(ctx, s, acct); err != nil {
		return err
	}
	// Check with ENGRAM_* variables applied, as the commands will run.
	if s, err = c.settings(); err != nil {
		return err
	}
	signedIn := fmt.Sprintf("Signed in as %s (%s).", acct.Handle, acct.DID)
	if exp := acct.Expires(); !exp.IsZero() {
		signedIn += fmt.Sprintf(" The sign-in lasts until %s.", exp.Local().Format("Jan 2 15:04"))
	}
	if len(s.Spaces) == 0 {
		c.print(map[string]any{"handle": acct.Handle, "did": acct.DID, "signIn": acct.SignIn, "expires": timeOrNil(acct.Expires()), "spaces": []any{}}, func(w io.Writer) {
			fmt.Fprintf(w, "%s Settings saved to %s.\n\nNext:\n  engram spaces              the spaces this account belongs to\n  engram spaces add <URI>    use one\n  engram create <name>       make a new space\n", signedIn, c.configPath)
		})
		return nil
	}
	if !c.json {
		fmt.Fprintf(c.out, "%s Settings saved to %s.\n\nChecking the spaces…\n", signedIn, c.configPath)
	}
	// Offer to pull a missing model, unless the output is for a program.
	st := c.check(ctx, s, checkSpace, !c.json || *yes, *yes)
	c.print(st, func(w io.Writer) {
		st.write(w)
		if st.OK {
			fmt.Fprint(w, `
Ready. Try:
  engram remember "The deploy runbook lives in docs/deploy.md" -t ops
  engram recall "how do we deploy"

For MCP clients:
  {"mcpServers": {"engram": {"command": "engram-mcp"}}}
`)
		}
	})
	if !st.OK {
		return reportedError{errors.New("signed in, but not ready yet: see above")}
	}
	return nil
}

// cmdLogout ends the sign-in: the account's server revokes an OAuth
// session, and the settings forget the account (and any saved password).
// Spaces and embedding settings stay.
func (c *cli) cmdLogout(ctx context.Context, args []string) error {
	if _, err := parse(flag.NewFlagSet("logout", flag.ContinueOnError), args); err != nil {
		return err
	}
	s, err := c.fileSettings()
	if err != nil {
		return err
	}
	old := s.Account
	if old.SignIn == "" {
		c.print(map[string]any{"signedOut": false}, func(w io.Writer) { fmt.Fprintln(w, "Not signed in.") })
		return nil
	}
	if old.SignIn == agent.SignInOAuth && c.revoke != nil {
		if err := c.revoke(ctx, old); err != nil {
			fmt.Fprintf(c.err, "(couldn't end the sign-in at the account's server: %v; forgetting it here anyway)\n", agent.Explain(err))
		}
	}
	s.Account = agent.Account{}
	if err := s.Save(c.configPath); err != nil {
		return err
	}
	c.print(map[string]any{"signedOut": true, "handle": old.Handle}, func(w io.Writer) {
		fmt.Fprintf(w, "Signed out of %s. engram login signs in again.\n", old.Handle)
	})
	if c.getenv("ENGRAM_IDENTIFIER") != "" {
		fmt.Fprintln(c.err, "(ENGRAM_IDENTIFIER and ENGRAM_PASSWORD are set, so commands still sign in with them.)")
	}
	return nil
}

// saveAccount saves a new sign-in, then ends the one it replaces so its
// tokens don't linger.
func (c *cli) saveAccount(ctx context.Context, s agent.Settings, acct agent.Account) (agent.Settings, error) {
	old := s.Account
	s.Account = acct
	if err := s.CheckAccount(); err != nil {
		return s, err
	}
	if err := s.Save(c.configPath); err != nil {
		return s, err
	}
	if acct.SignIn == agent.SignInPassword {
		fmt.Fprintf(c.err, "The account's password is saved in %s (readable only by you).\n", c.configPath)
	}
	if old.SignIn == agent.SignInOAuth && old.SessionID != "" && old.SessionID != acct.SessionID && c.revoke != nil {
		if err := c.revoke(ctx, old); err != nil {
			fmt.Fprintf(c.err, "(couldn't end the previous sign-in: %v)\n", err)
		}
	}
	return s, nil
}

func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

// doSignIn signs in with OAuth, or with a password (prompting for it, or
// taking ENGRAM_PASSWORD).
func (c *cli) doSignIn(ctx context.Context, handle string, usePassword bool) (agent.Account, error) {
	if !usePassword {
		return c.signIn(ctx, handle, "", c.lineInput())
	}
	pw := c.getenv("ENGRAM_PASSWORD")
	if pw == "" {
		prompt := fmt.Sprintf("Password for %s: ", handle)
		var err error
		if c.readSecret != nil {
			pw, err = c.readSecret(prompt)
		} else {
			fmt.Fprint(c.err, prompt)
			pw, err = c.readLine()
		}
		if err != nil {
			return agent.Account{}, err
		}
	}
	if pw == "" {
		return agent.Account{}, errors.New("no password given")
	}
	return c.signIn(ctx, handle, pw, nil)
}

func (c *cli) ask(prompt string) (string, error) {
	fmt.Fprint(c.err, prompt)
	line, err := c.readLine()
	if line = strings.TrimSpace(line); line != "" {
		return line, nil
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return "", errors.New("nothing entered")
}

// ---- checks ----

type checkResult struct {
	OK      bool         `json:"ok"`
	Account string       `json:"account"`
	DID     string       `json:"did,omitempty"`
	SignIn  string       `json:"signIn,omitempty"`
	Expires any          `json:"signInExpires,omitempty"`
	Appview string       `json:"appview"`
	Spaces  []spaceCheck `json:"spaces"`
	// Problems are the account's; each space has its own.
	Problems []string `json:"problems,omitempty"`
	// Warnings need attention soon but don't stop anything working.
	Warnings []string `json:"warnings,omitempty"`
}

type spaceCheck struct {
	Name     string   `json:"name"`
	URI      string   `json:"uri"`
	Default  bool     `json:"default,omitempty"`
	Model    string   `json:"model,omitempty"`
	Memories *int     `json:"memories,omitempty"`
	Indexing string   `json:"indexing,omitempty"`
	Problems []string `json:"problems,omitempty"`
}

func (r checkResult) write(w io.Writer) {
	fmt.Fprintf(w, "Account:  %s %s\n", r.Account, r.DID)
	if exp, ok := r.Expires.(string); ok {
		fmt.Fprintf(w, "Sign-in:  %s, until %s\n", r.SignIn, exp)
	} else if r.SignIn != "" {
		fmt.Fprintf(w, "Sign-in:  %s\n", r.SignIn)
	}
	fmt.Fprintf(w, "Appview:  %s\n", r.Appview)
	for _, p := range r.Problems {
		fmt.Fprintf(w, "\n✗ %s\n", p)
	}
	for _, sp := range r.Spaces {
		def := ""
		if sp.Default {
			def = " (default)"
		}
		fmt.Fprintf(w, "\nSpace:    %s%s\n          %s\n", sp.Name, def, sp.URI)
		if sp.Model != "" {
			fmt.Fprintf(w, "Model:    %s\n", sp.Model)
		}
		if sp.Memories != nil {
			fmt.Fprintf(w, "Memories: %d\n", *sp.Memories)
		}
		if sp.Indexing != "" {
			fmt.Fprintf(w, "Indexing: %s\n", sp.Indexing)
		}
		for _, p := range sp.Problems {
			fmt.Fprintf(w, "✗ %s\n", p)
		}
	}
	for _, p := range r.Warnings {
		fmt.Fprintf(w, "\n! %s\n", p)
	}
	if r.OK {
		fmt.Fprintln(w, "\n✓ Everything works.")
	}
}

func timeOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Format(time.RFC3339)
}

// check signs in and checks each space (or only the one named): membership,
// its model, the local model, and the appview. With offerPull set it offers
// to fetch a missing model.
func (c *cli) check(ctx context.Context, s agent.Settings, only string, offerPull, yes bool) checkResult {
	r := checkResult{Account: s.Account.Handle, DID: s.Account.DID, SignIn: s.Account.SignIn, Appview: s.AppviewURL,
		Expires: timeOrNil(s.Account.Expires()), Spaces: []spaceCheck{}}
	if exp := s.Account.Expires(); !exp.IsZero() && time.Until(exp) < 3*24*time.Hour {
		r.Warnings = append(r.Warnings, "the sign-in ends soon: run `engram login` to renew it")
	}
	a, err := c.open(ctx, s)
	if err != nil {
		r.Problems = append(r.Problems, fmt.Sprintf("signing in: %v", agent.Explain(err)))
		return r
	}
	r.DID = a.Client.DID().String()
	def, _ := s.Default()
	r.OK = true
	for _, e := range s.Spaces {
		if only != "" && e.Name != only {
			continue
		}
		sc := c.checkSpace(ctx, a, s, e, offerPull, yes)
		sc.Default = e.URI == def.URI
		r.OK = r.OK && len(sc.Problems) == 0
		r.Spaces = append(r.Spaces, sc)
	}
	return r
}

func (c *cli) checkSpace(ctx context.Context, a *agent.Spaces, s agent.Settings, e agent.SpaceEntry, offerPull, yes bool) spaceCheck {
	r := spaceCheck{Name: e.Name, URI: e.URI}
	fail := func(format string, args ...any) spaceCheck {
		r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
		return r
	}
	sp, _, err := a.Agent(e.URI)
	if err != nil {
		return fail("%v", err)
	}
	if _, err := sp.Client.Credential(ctx, e.URI); err != nil {
		return fail("this account can't read the space: %v\n  The space's authority needs to add %s as a member who can write (under Manage in the web app).", err, sp.Client.DID())
	}
	cfg, err := sp.Config(ctx, true)
	if err != nil {
		return fail("%v", err)
	}
	r.Model = fmt.Sprintf("%s (%d dimensions)", cfg.Model, cfg.Dims)
	if _, err := a.Provider.For(ctx, cfg.ModelInfo); err != nil {
		// Ollama doesn't have the model at all (rather than another version).
		var mm *embed.ModelMismatchError
		missing := errors.As(err, &mm) && mm.Local == "" && s.Embed.Digest == ""
		if missing && offerPull && c.pull != nil && (yes || c.confirm(fmt.Sprintf("The space uses %s, which this machine's Ollama doesn't have. Pull it now? [Y/n] ", cfg.Model))) {
			if perr := c.pull(ctx, cfg.Model); perr != nil {
				return fail("pulling %s: %v", cfg.Model, perr)
			}
			_, err = a.Provider.For(ctx, cfg.ModelInfo)
		}
		if err != nil {
			return fail("the local embedding model can't be used: %v\n  Install the space's model (ollama pull %s), or point --embed-url at an endpoint that has it.", err, cfg.Model)
		}
	}
	var st struct {
		Memories int `json:"memories"`
		Access   *struct {
			State string `json:"state"`
		} `json:"access"`
	}
	if err := sp.Client.Query(ctx, sp.AppviewURL, e.URI, sp.AppviewDID, "garden.engram.getSpaceStatus", url.Values{"space": {e.URI}}, &st); err != nil {
		return fail("the appview at %s can't search the space yet: %v\n  The space's authority needs to let the appview index it (in the web app).", s.AppviewURL, err)
	}
	r.Memories = &st.Memories
	if st.Access != nil {
		r.Indexing = st.Access.State
		if st.Access.State != "granted" {
			r.Problems = append(r.Problems, "the appview isn't allowed to read the space ("+st.Access.State+"), so new memories won't be searchable: the space's authority must approve indexing, with `engram index --space "+e.Name+"` or in the web app")
		}
	}
	return r
}

func (c *cli) confirm(prompt string) bool {
	fmt.Fprint(c.err, prompt)
	line, _ := c.readLine()
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "" || line == "y" || line == "yes"
}

func (c *cli) cmdStatus(ctx context.Context, args []string) error {
	if _, err := parse(flag.NewFlagSet("status", flag.ContinueOnError), args); err != nil {
		return err
	}
	s, err := c.settings()
	if err != nil {
		return err
	}
	if err := s.Check(); err != nil {
		return err
	}
	r := c.check(ctx, s, "", false, false)
	c.print(r, r.write)
	if !r.OK {
		return reportedError{errors.New("not ready: see above")}
	}
	return nil
}

// ---- memories ----

func (c *cli) cmdRemember(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("remember", flag.ContinueOnError)
	var tags stringsFlag
	fs.Var(&tags, "t", "a tag (repeatable)")
	fs.Var(&tags, "tag", "a tag (repeatable)")
	source := fs.String("source", "", "where this came from: a URL, file, PR or task")
	sp := fs.String("space", "", "the space to store it in, by name or URI (default: the default space)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	text := strings.Join(pos, " ")
	if text == "" || text == "-" {
		raw, err := io.ReadAll(io.LimitReader(c.reader(), 64<<10))
		if err != nil {
			return err
		}
		text = string(raw)
	}
	if strings.TrimSpace(text) == "" {
		return usageError{errors.New("nothing to remember: give the text as arguments or on stdin")}
	}
	a, _, err := c.agent(ctx)
	if err != nil {
		return err
	}
	out, err := a.Remember(ctx, agent.RememberIn{Text: text, Tags: tags, Source: *source, Space: *sp})
	if err != nil {
		return err
	}
	c.print(out, func(w io.Writer) {
		fmt.Fprintf(w, "Remembered: %s\n", out.URI)
		if out.Note != "" {
			fmt.Fprintf(w, "\n(%s)\n", out.Note)
		}
	})
	return nil
}

func (c *cli) cmdRecall(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("recall", flag.ContinueOnError)
	var tags stringsFlag
	fs.Var(&tags, "t", "only memories with this tag (repeatable)")
	fs.Var(&tags, "tag", "only memories with this tag (repeatable)")
	n := fs.Int("n", 10, "how many results (1-50)")
	author := fs.String("author", "", "only memories by this agent DID")
	since := fs.String("since", "", "only memories created at or after this RFC 3339 time")
	sp := fs.String("space", agent.AllSpaces, "search only this space, by name or URI (default: every space)")
	mode := fs.String("mode", "", "hybrid (default: meaning and exact words), vector (meaning only) or keyword (exact words only)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	q := strings.Join(pos, " ")
	if strings.TrimSpace(q) == "" {
		return usageError{errors.New("what should I look for? engram recall <query>")}
	}
	a, _, err := c.agent(ctx)
	if err != nil {
		return err
	}
	out, err := a.Recall(ctx, agent.RecallIn{Query: q, Limit: *n, Author: *author, Tags: tags, Since: *since, Space: *sp, Mode: *mode})
	if err != nil {
		return err
	}
	c.print(out, func(w io.Writer) { writeMemories(w, out) })
	return nil
}

func (c *cli) cmdList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	var tags stringsFlag
	fs.Var(&tags, "t", "only memories with this tag (repeatable)")
	fs.Var(&tags, "tag", "only memories with this tag (repeatable)")
	n := fs.Int("n", 25, "how many (1-100)")
	author := fs.String("author", "", "only memories by this agent DID")
	mine := fs.Bool("mine", false, "only this agent's memories")
	cursor := fs.String("cursor", "", "continue from a previous page")
	sp := fs.String("space", "", "the space to list, by name or URI (default: the default space)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	a, _, err := c.agent(ctx)
	if err != nil {
		return err
	}
	if *mine {
		*author = a.Client.DID().String()
	}
	out, err := a.List(ctx, agent.ListIn{Limit: *n, Cursor: *cursor, Author: *author, Tags: tags, Space: *sp})
	if err != nil {
		return err
	}
	c.print(out, func(w io.Writer) {
		writeMemories(w, out)
		if out.Cursor != "" {
			more := "engram list --cursor " + out.Cursor
			if *sp != "" {
				more += " --space " + *sp
			}
			fmt.Fprintf(w, "More: %s\n", more)
		}
	})
	return nil
}

func (c *cli) cmdGet(ctx context.Context, args []string) error {
	pos, err := parse(flag.NewFlagSet("get", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{errors.New("engram get <memory URI>")}
	}
	a, _, err := c.agent(ctx)
	if err != nil {
		return err
	}
	out, err := a.Get(ctx, agent.GetIn{URI: pos[0]})
	if err != nil {
		return err
	}
	c.print(out, func(w io.Writer) { writeMemories(w, agent.MemoriesOut{Memories: []agent.Memory{out.Memory}}) })
	return nil
}

func (c *cli) cmdForget(ctx context.Context, args []string) error {
	pos, err := parse(flag.NewFlagSet("forget", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{errors.New("engram forget <memory URI>")}
	}
	a, _, err := c.agent(ctx)
	if err != nil {
		return err
	}
	out, err := a.Forget(ctx, agent.ForgetIn{URI: pos[0]})
	if err != nil {
		return err
	}
	c.print(out, func(w io.Writer) { fmt.Fprintf(w, "Forgot %s\n", out.Deleted) })
	return nil
}

func writeMemories(w io.Writer, out agent.MemoriesOut) {
	if len(out.Memories) == 0 {
		fmt.Fprintln(w, "No memories found.")
	}
	for i, m := range out.Memories {
		if i > 0 {
			fmt.Fprintln(w)
		}
		head := m.CreatedAt
		if t, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil {
			head = t.Local().Format("2006-01-02 15:04")
		}
		head += "  " + m.Author
		if m.Space != "" {
			head += "  in " + m.Space
		}
		if m.Similarity != nil {
			head = fmt.Sprintf("[%d] %s", *m.Similarity, head)
		}
		fmt.Fprintln(w, head)
		if len(m.Tags) > 0 {
			fmt.Fprintf(w, "tags: %s\n", strings.Join(m.Tags, ", "))
		}
		fmt.Fprintln(w, strings.TrimSpace(m.Text))
		if m.Source != "" {
			fmt.Fprintf(w, "source: %s\n", m.Source)
		}
		if m.Match != nil && m.Match.Keyword != nil && len(m.Match.Keyword.Terms) > 0 {
			var terms []string
			for _, t := range m.Match.Keyword.Terms {
				s := t.Term
				if t.Kind != "exact" {
					s += " (" + t.Kind + ")"
				}
				if t.Field != "text" {
					s += " in " + t.Field
				}
				terms = append(terms, s)
			}
			fmt.Fprintf(w, "matched: %s\n", strings.Join(terms, ", "))
		}
		fmt.Fprintln(w, m.URI)
	}
	if out.Note != "" {
		fmt.Fprintf(w, "\n(%s)\n", out.Note)
	}
}

// ollamaPull runs `ollama pull`, when ollama is installed.
func ollamaPull(stderr io.Writer) func(context.Context, string) error {
	path, err := exec.LookPath("ollama")
	if err != nil {
		return nil
	}
	return func(ctx context.Context, model string) error {
		cmd := exec.CommandContext(ctx, path, "pull", model)
		cmd.Stdout, cmd.Stderr = stderr, stderr
		return cmd.Run()
	}
}
