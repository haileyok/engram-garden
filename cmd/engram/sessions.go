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
	"time"

	"github.com/haileyok/engram-garden/internal/agent"
	"github.com/haileyok/engram-garden/internal/lex"
)

const sessionUsage = "engram sessions list | pull <id> [--to file] | push <file> [--id id] [--title t] | rm <id>   (--space picks a space)"

// cmdSessions keeps agent sessions in the spaces: where each one stands, and
// its transcript, so it can be picked up on another machine.
func (c *cli) cmdSessions(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError{errors.New(sessionUsage)}
	}
	switch args[0] {
	case "list", "ls":
		return c.cmdSessionsList(ctx, args[1:])
	case "pull", "get", "show":
		return c.cmdSessionsPull(ctx, args[1:])
	case "push", "put", "save":
		return c.cmdSessionsPush(ctx, args[1:])
	case "rm", "delete":
		return c.cmdSessionsRm(ctx, args[1:])
	}
	return usageError{fmt.Errorf("unknown sessions command %q: %s", args[0], sessionUsage)}
}

func (c *cli) cmdSessionsList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessions list", flag.ContinueOnError)
	sp := fs.String("space", "", "only this space")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	s, _, err := c.agent(ctx)
	if err != nil {
		return err
	}
	out, err := s.ListSessions(ctx, agent.ListSessionsIn{Space: *sp})
	if err != nil {
		return err
	}
	me := s.Client.DID().String()
	c.print(out, func(w io.Writer) {
		if len(out.Sessions) == 0 {
			fmt.Fprintln(w, "No stored sessions. A harness pushes one with push_session, or: engram sessions push <file.jsonl>")
		}
		for _, si := range out.Sessions {
			where := si.Machine
			if si.Branch != "" {
				where += " " + si.Branch
			}
			if where != "" {
				where = "(" + strings.TrimSpace(where) + ")"
			}
			by := ""
			if si.Author != me {
				by = " by " + si.Author
			}
			stored := "no transcript"
			if si.Head != nil && si.Head.Bytes > 0 {
				stored = fmt.Sprintf("%d entries, %s", si.Head.Entries, humanBytes(si.Head.Bytes))
				if si.Mode != "" {
					stored += " in " + si.Mode
				}
			}
			fields := []string{si.Space, where, "updated " + si.UpdatedAt, stored}
			fmt.Fprintf(w, "%s  %s%s\n  %s\n", si.SessionID, si.Title, by, strings.Join(slices.DeleteFunc(fields, func(f string) bool { return f == "" }), " · "))
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
	sp := fs.String("space", "", "the space (default: wherever the session is)")
	to := fs.String("to", "", "write the transcript here instead of to stdout")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{errors.New("engram sessions pull <id> [--to file] [--space s]")}
	}
	s, _, err := c.agent(ctx)
	if err != nil {
		return err
	}
	out, err := s.PullSession(ctx, agent.GetSessionIn{Session: pos[0], Space: *sp})
	if err != nil {
		return err
	}
	if *to != "" {
		if err := os.WriteFile(*to, []byte(out.Data), 0o600); err != nil {
			return err
		}
		c.print(map[string]string{"wrote": *to, "session": out.Session.SessionID}, func(w io.Writer) {
			fmt.Fprintf(w, "Wrote %s (%s) from %s\n", *to, humanBytes(int64(len(out.Data))), out.Space)
		})
		return nil
	}
	// The transcript goes to stdout as it is, so it can be piped.
	if _, err := io.WriteString(c.out, out.Data); err != nil {
		return err
	}
	if !strings.HasSuffix(out.Data, "\n") && out.Data != "" {
		fmt.Fprintln(c.out)
	}
	return nil
}

func (c *cli) cmdSessionsPush(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessions push", flag.ContinueOnError)
	sp := fs.String("space", "", "the space (default: the default space)")
	id := fs.String("id", "", "the session id (default: from the file's name)")
	title := fs.String("title", "", "a short name for the work")
	summary := fs.String("summary", "", "markdown: the goal, what's done and the next steps")
	cwd := fs.String("cwd", "", "the working directory the session ran in")
	machine := fs.String("machine", "", "the machine's host name")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{errors.New("engram sessions push <file> [--id id] [--title t] [--space s]")}
	}
	data, err := os.ReadFile(pos[0])
	if err != nil {
		return err
	}
	if *id == "" {
		*id = sessionIDFromFile(pos[0])
	}
	if *title == "" {
		*title = *id
	}
	s, _, err := c.agent(ctx)
	if err != nil {
		return err
	}
	rec, err := sessionRecord(*id, *title, *summary, *cwd, *machine)
	if err != nil {
		return err
	}
	out, err := s.PushSession(ctx, agent.PushSessionIn{
		Space:   *sp,
		Replace: true,
		Data:    string(data),
		Session: rec,
	})
	if err != nil {
		return err
	}
	c.print(out, func(w io.Writer) {
		fmt.Fprintf(w, "Stored %s: %d entries, %s in %s\n", out.Session, out.Entries, humanBytes(out.Bytes), out.Mode)
		if out.Note != "" {
			fmt.Fprintf(w, "Note: %s\n", out.Note)
		}
	})
	return nil
}

func (c *cli) cmdSessionsRm(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessions rm", flag.ContinueOnError)
	sp := fs.String("space", "", "the space (default: wherever your session is)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{errors.New("engram sessions rm <id> [--space s]")}
	}
	s, _, err := c.agent(ctx)
	if err != nil {
		return err
	}
	out, err := s.DeleteSession(ctx, agent.DeleteSessionIn{Session: pos[0], Space: *sp})
	if err != nil {
		return err
	}
	c.print(out, func(w io.Writer) { fmt.Fprintf(w, "Deleted %s\n", out.Deleted) })
	return nil
}

// sessionRecord is the manifest a CLI push writes: what the flags said, and
// what this machine can see for itself.
func sessionRecord(id, title, summary, cwd, machine string) (lex.Session, error) {
	if !lex.ValidSessionID(id) {
		return lex.Session{}, fmt.Errorf("%q isn't a session id (use --id)", id)
	}
	now := time.Now().UTC()
	rec := lex.Session{
		SessionID: id, Title: title, Summary: summary, Status: "ended",
		Cwd: cwd, Machine: machine, UpdatedAt: now.Format("2006-01-02T15:04:05.000Z"),
	}
	if rec.Machine == "" {
		rec.Machine, _ = os.Hostname()
	}
	if rec.Cwd == "" {
		rec.Cwd, _ = os.Getwd()
	}
	return rec, nil
}

// sessionIDFromFile guesses a session id from a harness's file name, which
// looks like 2026-10-22T00-21-19-078Z_01a160fb-….jsonl.
func sessionIDFromFile(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if i := strings.LastIndex(base, "_"); i >= 0 && i+1 < len(base) {
		base = base[i+1:]
	}
	return base
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
