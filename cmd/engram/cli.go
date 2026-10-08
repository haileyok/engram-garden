package main

import (
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
	open func(context.Context, agent.Settings) (*agent.Agent, error)
	// signIn signs in to an account: OAuth, or password when password is set.
	signIn func(ctx context.Context, handle, password string) (agent.Account, error)
	// readSecret reads a password without echoing it.
	readSecret func(prompt string) (string, error)
	// pull fetches an embedding model (`ollama pull`); nil when unavailable.
	pull func(ctx context.Context, model string) error

	json bool
}

const usage = `engram: shared memory for agents, on Engram Garden

Set up once:
  engram init --space <space URI>     sign in as the agent's account and check everything works
  engram login                        sign in again (OAuth sign-ins last two weeks)

Use:
  engram remember <text> [-t tag]... [--source s]   store a memory (text from stdin if omitted)
  engram recall <query> [-n 10] [-t tag]...          search every agent's memories by meaning
  engram list [-n 25] [--author did] [-t tag]...     newest memories first
  engram get <uri>                                   one memory
  engram forget <uri>                                delete one of your own memories
  engram status                                      account, space, model and sign-in

Add --json to any command for machine-readable output. MCP clients can run
engram-mcp, which uses the same settings.

Settings: %s
`

func (c *cli) run(ctx context.Context, args []string) int {
	// --json and --config may appear anywhere.
	var rest []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--json" || a == "-json":
			c.json = true
		case a == "--config" && i+1 < len(args):
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
		"init": c.cmdInit, "login": c.cmdLogin, "remember": c.cmdRemember, "recall": c.cmdRecall,
		"list": c.cmdList, "get": c.cmdGet, "forget": c.cmdForget, "status": c.cmdStatus,
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
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func (c *cli) settings() (agent.Settings, error) {
	return agent.LoadSettings(c.configPath, c.getenv)
}

func (c *cli) agent(ctx context.Context) (*agent.Agent, agent.Settings, error) {
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

func (c *cli) cmdInit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	spaceURI := fs.String("space", "", "the memory space's URI (at://…/space/garden.engram.space/…)")
	handle := fs.String("handle", "", "the agent's account handle")
	password := fs.Bool("password", false, "sign in with the account's password instead of OAuth (for machines without a browser)")
	appview := fs.String("appview", "", "the appview's URL (default "+agent.DefaultAppviewURL+")")
	embedURL := fs.String("embed-url", "", "OpenAI-compatible embedding endpoint (default Ollama at http://localhost:11434/v1)")
	yes := fs.Bool("yes", false, "answer yes to questions (pulling the model)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	s, err := c.settings()
	if err != nil {
		return err
	}
	if *spaceURI != "" {
		s.Space = *spaceURI
	}
	if *appview != "" {
		s.SetAppview(*appview)
	}
	if *embedURL != "" {
		s.Embed.URL = *embedURL
	}
	if s.Space == "" {
		if s.Space, err = c.ask("Memory space URI (from the space's page in the web app): "); err != nil {
			return err
		}
	}
	if *handle == "" {
		*handle = s.Account.Handle
	}
	if *handle == "" {
		if *handle, err = c.ask("The agent's account handle: "); err != nil {
			return err
		}
	}
	acct, err := c.doSignIn(ctx, *handle, *password)
	if err != nil {
		return err
	}
	s.Account = acct
	if err := s.Check(); err != nil {
		return err
	}
	if err := s.Save(c.configPath); err != nil {
		return err
	}
	if !c.json {
		fmt.Fprintf(c.out, "Signed in as %s (%s). Settings saved to %s.\n\nChecking the space…\n", acct.Handle, acct.DID, c.configPath)
	}
	st := c.check(ctx, s, *yes)
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
		return errors.New("set up, but not ready yet: see above")
	}
	return nil
}

func (c *cli) cmdLogin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	password := fs.Bool("password", false, "sign in with the account's password instead of OAuth")
	handle := fs.String("handle", "", "sign in as a different account")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	s, err := c.settings()
	if err != nil {
		return err
	}
	h := *handle
	if h == "" {
		h = s.Account.Handle
	}
	if h == "" {
		return errors.New("no account set up yet: run `engram init`")
	}
	// Keep the way the account signed in last time unless told otherwise.
	usePassword := *password || (s.Account.SignIn == agent.SignInPassword && !flagSet(fs, "password"))
	acct, err := c.doSignIn(ctx, h, usePassword)
	if err != nil {
		return err
	}
	s.Account = acct
	if err := s.Save(c.configPath); err != nil {
		return err
	}
	c.print(map[string]any{"handle": acct.Handle, "did": acct.DID, "signIn": acct.SignIn, "expires": timeOrNil(acct.Expires())}, func(w io.Writer) {
		fmt.Fprintf(w, "Signed in as %s (%s).", acct.Handle, acct.DID)
		if exp := acct.Expires(); !exp.IsZero() {
			fmt.Fprintf(w, " The sign-in lasts until %s.", exp.Local().Format("Jan 2 15:04"))
		}
		fmt.Fprintln(w)
	})
	return nil
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
		return c.signIn(ctx, handle, "")
	}
	pw := c.getenv("ENGRAM_PASSWORD")
	if pw == "" {
		var err error
		if pw, err = c.readSecret(fmt.Sprintf("Password for %s: ", handle)); err != nil {
			return agent.Account{}, err
		}
	}
	if pw == "" {
		return agent.Account{}, errors.New("no password given")
	}
	return c.signIn(ctx, handle, pw)
}

func (c *cli) ask(prompt string) (string, error) {
	fmt.Fprint(c.err, prompt)
	var line string
	_, err := fmt.Fscanln(c.in, &line)
	line = strings.TrimSpace(line)
	if line == "" {
		if err == nil || errors.Is(err, io.EOF) || err.Error() == "unexpected newline" {
			return "", errors.New("nothing entered")
		}
		return "", err
	}
	return line, nil
}

// ---- checks ----

type checkResult struct {
	OK       bool     `json:"ok"`
	Account  string   `json:"account"`
	DID      string   `json:"did,omitempty"`
	SignIn   string   `json:"signIn,omitempty"`
	Expires  any      `json:"signInExpires,omitempty"`
	Space    string   `json:"space"`
	Appview  string   `json:"appview"`
	Model    string   `json:"model,omitempty"`
	Memories *int     `json:"memories,omitempty"`
	Indexing string   `json:"indexing,omitempty"`
	Problems []string `json:"problems,omitempty"`
	// Warnings need attention soon but don't stop anything working.
	Warnings []string `json:"warnings,omitempty"`
}

func (r checkResult) write(w io.Writer) {
	fmt.Fprintf(w, "Account:  %s %s\n", r.Account, r.DID)
	if exp, ok := r.Expires.(string); ok {
		fmt.Fprintf(w, "Sign-in:  %s, until %s\n", r.SignIn, exp)
	} else if r.SignIn != "" {
		fmt.Fprintf(w, "Sign-in:  %s\n", r.SignIn)
	}
	fmt.Fprintf(w, "Space:    %s\nAppview:  %s\n", r.Space, r.Appview)
	if r.Model != "" {
		fmt.Fprintf(w, "Model:    %s\n", r.Model)
	}
	if r.Memories != nil {
		fmt.Fprintf(w, "Memories: %d\n", *r.Memories)
	}
	if r.Indexing != "" {
		fmt.Fprintf(w, "Indexing: %s\n", r.Indexing)
	}
	for _, p := range r.Problems {
		fmt.Fprintf(w, "\n✗ %s\n", p)
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

// check signs in and checks the space: membership, its model, the local
// model, and the appview. With pull set it offers to fetch a missing model.
func (c *cli) check(ctx context.Context, s agent.Settings, yes bool) checkResult {
	r := checkResult{Account: s.Account.Handle, DID: s.Account.DID, SignIn: s.Account.SignIn, Space: s.Space, Appview: s.AppviewURL, Expires: timeOrNil(s.Account.Expires())}
	fail := func(format string, args ...any) checkResult {
		r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
		return r
	}
	if exp := s.Account.Expires(); !exp.IsZero() && time.Until(exp) < 3*24*time.Hour {
		r.Warnings = append(r.Warnings, "the sign-in ends soon: run `engram login` to renew it")
	}
	a, err := c.open(ctx, s)
	if err != nil {
		return fail("signing in: %v", agent.Explain(err))
	}
	r.DID = a.Client.DID().String()
	if _, err := a.Client.Credential(ctx, s.Space); err != nil {
		return fail("this account can't read the space: %v\n  The space's authority needs to add %s as a member who can write (under Manage in the web app).", err, r.DID)
	}
	cfg, err := a.Config(ctx, true)
	if err != nil {
		return fail("%v", err)
	}
	r.Model = fmt.Sprintf("%s (%d dimensions)", cfg.Model, cfg.Dims)
	if _, err := a.Provider.For(ctx, cfg.ModelInfo); err != nil {
		// Ollama doesn't have the model at all (rather than another version).
		var mm *embed.ModelMismatchError
		missing := errors.As(err, &mm) && mm.Local == "" && s.Embed.Digest == ""
		if missing && c.pull != nil && (yes || c.confirm(fmt.Sprintf("The space uses %s, which this machine's Ollama doesn't have. Pull it now? [Y/n] ", cfg.Model))) {
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
	if err := a.Client.Query(ctx, s.AppviewURL, s.Space, s.AppviewDID, "garden.engram.getSpaceStatus", url.Values{"space": {s.Space}}, &st); err != nil {
		return fail("the appview at %s can't search the space yet: %v\n  The space's authority needs to let the appview index it (in the web app).", s.AppviewURL, err)
	}
	r.Memories = &st.Memories
	if st.Access != nil {
		r.Indexing = st.Access.State
		if st.Access.State != "granted" {
			r.Problems = append(r.Problems, "the appview isn't allowed to read the space ("+st.Access.State+"), so new memories won't be searchable: the space's authority can let it index the space in the web app")
		}
	}
	r.OK = len(r.Problems) == 0
	return r
}

func (c *cli) confirm(prompt string) bool {
	fmt.Fprint(c.err, prompt)
	var line string
	_, _ = fmt.Fscanln(c.in, &line)
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
	r := c.check(ctx, s, false)
	c.print(r, r.write)
	if !r.OK {
		return errors.New("not ready: see above")
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
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	text := strings.Join(pos, " ")
	if text == "" || text == "-" {
		raw, err := io.ReadAll(io.LimitReader(c.in, 64<<10))
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
	out, err := a.Remember(ctx, agent.RememberIn{Text: text, Tags: tags, Source: *source})
	if err != nil {
		return err
	}
	c.print(out, func(w io.Writer) { fmt.Fprintf(w, "Remembered: %s\n", out.URI) })
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
	out, err := a.Recall(ctx, agent.RecallIn{Query: q, Limit: *n, Author: *author, Tags: tags, Since: *since})
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
	out, err := a.List(ctx, agent.ListIn{Limit: *n, Cursor: *cursor, Author: *author, Tags: tags})
	if err != nil {
		return err
	}
	c.print(out, func(w io.Writer) {
		writeMemories(w, out)
		if out.Cursor != "" {
			fmt.Fprintf(w, "More: engram list --cursor %s\n", out.Cursor)
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
