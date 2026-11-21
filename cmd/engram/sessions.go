package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bluesky-social/indigo/atproto/atclient"

	"github.com/haileyok/engram-garden/internal/agent"
)

// The archive: where agent sessions keep their transcripts, each written by
// the session's own identity. The principal governs it and reads all of it.
// See docs/design/sessions.md.

const archiveUsage = `engram archive create <name> [--model m]   make an archive (agents keep their sessions there), the agents' archive
engram archive model <model>               declare the model the appview embeds passages with
engram archive index [--stop]              let the appview index it (approve in the browser)`

const sessionUsage = `engram sessions search <query> [--kind k] [--repo r] [--file f] [--cmd c] [--author did] [--limit n]
engram sessions list [--author did]                 the sessions in the archive
engram sessions pull <id|uri> [--author did] [--to file]   one transcript, as complete JSONL lines
engram sessions rm <id|uri> [--author did]          delete a session (with its agent's identity file)`

func (c *cli) cmdArchive(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError{errors.New(archiveUsage)}
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("archive create", flag.ContinueOnError)
		model := fs.String("model", "", "declare this embedding model (an Ollama model name, e.g. nomic-embed-text)")
		dims := fs.Int("dims", 0, "vector size; only for the offline hashing provider")
		pos, err := parse(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return usageError{errors.New("usage: engram archive create <name> [--model m]")}
		}
		p, err := c.principal()
		if err != nil {
			return err
		}
		if err := p.CheckCreateArchive(); err != nil {
			return err
		}
		sess, err := c.principalSession(ctx, p)
		if err != nil {
			return fmt.Errorf("signing in as the principal: %w", err)
		}
		uri, err := agent.CreateArchive(ctx, sess, pos[0])
		if err != nil {
			return err
		}
		p.Archive = uri
		if err := p.Save(c.principalPath); err != nil {
			return err
		}
		next := "Next: engram archive model <model>, then engram archive index (let the appview index it)."
		if *model != "" {
			if err := c.declareArchiveModel(ctx, sess, uri, *model, *dims); err != nil {
				next = fmt.Sprintf("Declaring its model failed (%v): engram archive model <model>, then engram archive index.", err)
			} else {
				next = "Next: engram archive index (let the appview index it)."
			}
		}
		c.print(map[string]any{"archive": uri}, func(w io.Writer) {
			fmt.Fprintf(w, "Created the archive %s; new agents keep their sessions there.\n%s\n", uri, next)
		})
		return nil
	case "model":
		fs := flag.NewFlagSet("archive model", flag.ContinueOnError)
		dims := fs.Int("dims", 0, "vector size; only for the offline hashing provider")
		pos, err := parse(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return usageError{errors.New("usage: engram archive model <model>")}
		}
		p, err := c.principal()
		if err != nil {
			return err
		}
		if err := p.CheckArchive(); err != nil {
			return err
		}
		sess, err := c.principalSession(ctx, p)
		if err != nil {
			return fmt.Errorf("signing in as the principal: %w", err)
		}
		if err := c.declareArchiveModel(ctx, sess, p.Archive, pos[0], *dims); err != nil {
			return err
		}
		c.print(map[string]any{"archive": p.Archive, "model": pos[0]}, func(w io.Writer) {
			fmt.Fprintf(w, "The archive's passages are embedded with %s.\n", pos[0])
		})
		return nil
	case "index":
		fs := flag.NewFlagSet("archive index", flag.ContinueOnError)
		stop := fs.Bool("stop", false, "stop the appview indexing it")
		if _, err := parse(fs, args[1:]); err != nil {
			return err
		}
		p, err := c.principal()
		if err != nil {
			return err
		}
		if p.Archive == "" {
			return errors.New("no archive yet: engram archive create <name>")
		}
		s, err := c.settings()
		if err != nil {
			return err
		}
		mode := "grant"
		if *stop {
			mode = "stop"
		}
		link, err := agent.GrantLink(ctx, s.AppviewURL, c.provisionHTTP, p.Archive, mode)
		if err != nil {
			return err
		}
		c.print(map[string]any{"archive": p.Archive, "link": link}, func(w io.Writer) {
			fmt.Fprintf(w, "Open this and sign in as %s to approve:\n\n  %s\n", p.Account.Handle, link)
		})
		if c.browser != nil && !c.json {
			_ = c.browser(link)
		}
		return nil
	}
	return usageError{fmt.Errorf("unknown archive command %q:\n%s", args[0], archiveUsage)}
}

func (c *cli) declareArchiveModel(ctx context.Context, sess *atclient.APIClient, archive, model string, dims int) error {
	s, err := c.settings()
	if err != nil {
		return err
	}
	m, err := s.DescribeModel(ctx, model, dims)
	if err != nil {
		return err
	}
	_, err = agent.DeclareArchiveModel(ctx, sess, archive, m)
	return err
}

// cmdSessions reads the archive as the principal, who governs it.
func (c *cli) cmdSessions(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError{errors.New(sessionUsage)}
	}
	switch args[0] {
	case "search", "find":
		return c.cmdSessionsSearch(ctx, args[1:])
	case "list", "ls":
		return c.cmdSessionsList(ctx, args[1:])
	case "pull", "get", "show":
		return c.cmdSessionsPull(ctx, args[1:])
	case "rm", "delete":
		return c.cmdSessionsRm(ctx, args[1:])
	}
	return usageError{fmt.Errorf("unknown sessions command %q:\n%s", args[0], sessionUsage)}
}

// archiveReader opens the archive as the principal.
func (c *cli) archiveReader(ctx context.Context) (*agent.Archive, error) {
	p, err := c.principal()
	if err != nil {
		return nil, err
	}
	if err := p.CheckArchive(); err != nil {
		return nil, err
	}
	sess, err := c.principalSession(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("signing in as the principal: %w", err)
	}
	sc, err := c.spaceClient(sess)
	if err != nil {
		return nil, err
	}
	return &agent.Archive{Client: sc, Space: p.Archive}, nil
}

func (c *cli) cmdSessionsSearch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessions search", flag.ContinueOnError)
	var in agent.SearchSessionsIn
	fs.StringVar(&in.Kind, "kind", "", "turn, step, error, summary or session")
	fs.StringVar(&in.Repo, "repo", "", "only this repository")
	fs.StringVar(&in.File, "file", "", "only passages that touched this path")
	fs.StringVar(&in.Command, "cmd", "", "only passages that ran this program")
	fs.StringVar(&in.Author, "author", "", "only this agent's sessions (a DID)")
	fs.StringVar(&in.Mode, "mode", "", "hybrid, vector or keyword")
	fs.IntVar(&in.Limit, "limit", 10, "at most this many")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	in.Query = strings.Join(pos, " ")
	a, err := c.archiveReader(ctx)
	if err != nil {
		return err
	}
	s, err := c.settings()
	if err != nil {
		return err
	}
	a.AppviewURL, a.AppviewDID = s.AppviewURL, s.AppviewDID
	out, err := a.SearchSessions(ctx, in)
	if err != nil {
		return err
	}
	c.print(out, func(w io.Writer) {
		if len(out.Passages) == 0 {
			fmt.Fprintln(w, "No passages matched.")
		}
		for i, p := range out.Passages {
			fmt.Fprintf(w, "[%d] %s · %s · %s %s\n    %s#%s\n", i+1, p.Kind, p.CreatedAt, p.Author, p.Repo, p.Session, p.Entry)
			for line := range strings.SplitSeq(p.Text, "\n") {
				fmt.Fprintf(w, "    %s\n", line)
			}
		}
	})
	return nil
}

func (c *cli) cmdSessionsList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessions list", flag.ContinueOnError)
	author := fs.String("author", "", "only this agent's sessions (a DID)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	a, err := c.archiveReader(ctx)
	if err != nil {
		return err
	}
	out, err := a.ListSessions(ctx, agent.ListSessionsIn{Author: *author})
	if err != nil {
		return err
	}
	c.print(out, func(w io.Writer) {
		if len(out.Sessions) == 0 {
			fmt.Fprintln(w, "No sessions in the archive yet.")
		}
		for _, si := range out.Sessions {
			kind := si.Kind
			if si.Parent != "" {
				kind += " of " + si.Parent
			}
			where := strings.TrimSpace(si.Machine + " " + si.Branch)
			stored := "no transcript"
			if si.Head != nil && si.Head.Bytes > 0 {
				stored = fmt.Sprintf("%d entries, %s", si.Head.Entries, humanBytes(si.Head.Bytes))
			}
			fields := []string{si.Author, kind, si.Status, where, "updated " + si.UpdatedAt, stored}
			fmt.Fprintf(w, "%s  %s\n  %s\n", si.SessionID, si.Title, strings.Join(slices.DeleteFunc(fields, func(f string) bool { return f == "" }), " · "))
			if si.Cwd != "" {
				fmt.Fprintf(w, "  %s\n", si.Cwd)
			}
		}
		if out.Note != "" {
			fmt.Fprintf(w, "Note: %s\n", out.Note)
		}
	})
	return nil
}

func (c *cli) cmdSessionsPull(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessions pull", flag.ContinueOnError)
	author := fs.String("author", "", "the session's agent (a DID), when the ID isn't enough")
	to := fs.String("to", "", "write the transcript here instead of to stdout")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{errors.New("engram sessions pull <id|uri> [--author did] [--to file]")}
	}
	a, err := c.archiveReader(ctx)
	if err != nil {
		return err
	}
	out, err := a.PullSession(ctx, agent.GetSessionIn{Session: pos[0], Author: *author})
	if err != nil {
		return err
	}
	if *to != "" {
		if err := os.WriteFile(*to, []byte(out.Data), 0o600); err != nil {
			return err
		}
		c.print(map[string]string{"wrote": *to, "session": out.URI}, func(w io.Writer) {
			fmt.Fprintf(w, "Wrote %s (%s) from %s\n", *to, humanBytes(int64(len(out.Data))), out.URI)
		})
		return nil
	}
	// The transcript goes to stdout as it is, so it can be piped.
	_, err = io.WriteString(c.out, out.Data)
	return err
}

// cmdSessionsRm deletes a session as its own agent: the record is in the
// agent's repo, so only the agent's identity can delete it.
func (c *cli) cmdSessionsRm(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessions rm", flag.ContinueOnError)
	author := fs.String("author", "", "the session's agent (a DID), when the ID isn't enough")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{errors.New("engram sessions rm <id|uri> [--author did]")}
	}
	r, err := c.archiveReader(ctx)
	if err != nil {
		return err
	}
	found, err := r.PullSession(ctx, agent.GetSessionIn{Session: pos[0], Author: *author})
	if err != nil {
		return err
	}
	path := filepath.Join(c.identitiesDir, "claimed", found.Author+".json")
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s was written by %s, whose identity file (%s) isn't on this machine: only that agent can delete it", found.URI, found.Author, path)
	}
	s, err := c.settings()
	if err != nil {
		return err
	}
	s.Identity, s.Archive = path, r.Space
	sc, id, err := c.identityClient(ctx, s)
	if err != nil {
		return err
	}
	a, err := agent.NewArchive(sc, id, s)
	if err != nil {
		return err
	}
	out, err := a.DeleteSession(ctx, agent.DeleteSessionIn{Session: found.SessionID})
	if err != nil {
		return err
	}
	c.print(out, func(w io.Writer) { fmt.Fprintf(w, "Deleted %s\n", out.Deleted) })
	return nil
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f kB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
