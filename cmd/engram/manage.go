package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/haileyok/engram-garden/internal/agent"
	"github.com/haileyok/engram-garden/internal/lex"
)

// Running spaces the account governs: create, members, model, index.

// saveSpace adds a space to the settings file (not ENGRAM_* overrides).
func (c *cli) saveSpace(uri string) error {
	file, err := c.fileSettings()
	if err != nil {
		return err
	}
	if _, err := file.AddSpace(uri, ""); err != nil {
		return err
	}
	return file.Save(c.configPath)
}

func (c *cli) cmdCreate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	model := fs.String("model", "", "declare this embedding model (an Ollama model name, e.g. nomic-embed-text)")
	dims := fs.Int("dims", 0, "vector size, for the offline hashing provider only")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{errors.New("engram create <name> [--model nomic-embed-text]")}
	}
	a, _, err := c.signedIn(ctx)
	if err != nil {
		return err
	}
	out, err := a.CreateSpace(ctx, agent.CreateSpaceIn{Name: pos[0], Model: *model, Dims: *dims})
	if err != nil {
		return err
	}
	if err := c.saveSpace(out.Space.URI); err != nil {
		return err
	}
	file, _ := c.fileSettings()
	def, _ := file.Default()
	c.print(out, func(w io.Writer) {
		fmt.Fprintf(w, "Created %s\n  as %q", out.Space.URI, out.Space.Name)
		if def.URI == out.Space.URI {
			fmt.Fprint(w, ", your default space")
		}
		fmt.Fprintln(w, ".")
		if out.Model != nil {
			fmt.Fprintf(w, "Model: %s\n", *out.Model)
		}
		fmt.Fprintf(w, "\n%s\n", out.Next)
		fmt.Fprintf(w, "  engram members add <handle> --space %s\n  engram index --space %s\n", out.Space.Name, out.Space.Name)
	})
	return nil
}

// signedIn opens the account's spaces; unlike agent(), it needs no space
// set up.
func (c *cli) signedIn(ctx context.Context) (*agent.Spaces, agent.Settings, error) {
	s, err := c.settings()
	if err != nil {
		return nil, s, err
	}
	if err := s.CheckAccount(); err != nil {
		return nil, s, err
	}
	a, err := c.open(ctx, s)
	return a, s, err
}

func (c *cli) cmdMembers(ctx context.Context, args []string) error {
	sub := ""
	if len(args) > 0 && (args[0] == "add" || args[0] == "remove" || args[0] == "rm") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("members", flag.ContinueOnError)
	sp := fs.String("space", "", "a space you govern, by name or URI (default: the default space)")
	readOnly := fs.Bool("read-only", false, "let them recall but not remember")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	a, _, err := c.agent(ctx)
	if err != nil {
		return err
	}
	switch sub {
	case "add", "remove", "rm":
		if len(pos) != 1 {
			return usageError{fmt.Errorf("engram members %s <handle or DID> [--space s]", sub)}
		}
		var out agent.MemberOut
		if sub == "add" {
			out, err = a.AddMember(ctx, agent.AddMemberIn{Space: *sp, Member: pos[0], ReadOnly: *readOnly})
		} else {
			out, err = a.RemoveMember(ctx, agent.RemoveMemberIn{Space: *sp, Member: pos[0]})
		}
		if err != nil {
			return err
		}
		c.print(out, func(w io.Writer) {
			who := out.Member.DID
			if out.Member.Handle != "" {
				who = out.Member.Handle + " (" + out.Member.DID + ")"
			}
			switch {
			case sub != "add":
				fmt.Fprintf(w, "Removed %s from %s.\n", who, out.Space)
			case out.Member.Write:
				fmt.Fprintf(w, "%s can now recall and remember in %s.\n", who, out.Space)
			default:
				fmt.Fprintf(w, "%s can now recall (not remember) in %s.\n", who, out.Space)
			}
		})
		return nil
	}
	if len(pos) > 0 {
		return usageError{errors.New("engram members [--space s] | members add <handle> | members remove <handle>")}
	}
	out, err := a.ListMembers(ctx, agent.MembersIn{Space: *sp})
	if err != nil {
		return err
	}
	c.print(out, func(w io.Writer) {
		if len(out.Members) == 0 {
			fmt.Fprintf(w, "%s has no members yet: engram members add <handle>\n", out.Space)
			return
		}
		for _, m := range out.Members {
			access := "read and write"
			if !m.Write {
				access = "read only"
			}
			name := m.DID
			if m.Handle != "" {
				name = m.Handle + "  " + m.DID
			}
			fmt.Fprintf(w, "%s  (%s)\n", name, access)
		}
	})
	return nil
}

func (c *cli) cmdModel(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("model", flag.ContinueOnError)
	sp := fs.String("space", "", "the space, by name or URI (default: the default space)")
	set := fs.String("set", "", "declare this model (an Ollama model name)")
	next := fs.String("next", "", "start moving to this model; agents re-embed their memories in the background")
	promote := fs.Bool("promote", false, "finish the move: the next model becomes the space's model")
	cancel := fs.Bool("cancel", false, "abandon the move")
	dims := fs.Int("dims", 0, "vector size, for the offline hashing provider only")
	docPrefix := fs.String("document-prefix", "", "text before memories when embedding (default: the model's convention)")
	queryPrefix := fs.String("query-prefix", "", "text before queries when embedding (default: the model's convention)")
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usageError{fmt.Errorf("unknown argument %q", pos[0])}
	}
	in := agent.SetModelIn{Space: *sp, Dims: *dims, DocumentPrefix: *docPrefix, QueryPrefix: *queryPrefix}
	n := 0
	for _, chosen := range []bool{*set != "", *next != "", *promote, *cancel} {
		if chosen {
			n++
		}
	}
	switch {
	case n > 1:
		return usageError{errors.New("choose one of --set, --next, --promote, --cancel")}
	case *set != "":
		in.Action, in.Model = string(lex.Declare), *set
	case *next != "":
		in.Action, in.Model = string(lex.StartNext), *next
	case *promote:
		in.Action = string(lex.Promote)
	case *cancel:
		in.Action = string(lex.CancelNext)
	}
	a, _, err := c.agent(ctx)
	if err != nil {
		return err
	}
	var out agent.ModelOut
	if in.Action == "" {
		out, err = a.Model(ctx, agent.MembersIn{Space: *sp})
	} else {
		out, err = a.SetModel(ctx, in)
	}
	if err != nil {
		return err
	}
	c.print(out, func(w io.Writer) {
		if out.Model == nil {
			fmt.Fprintf(w, "%s hasn't declared a model: engram model --set <model>\n", out.Space)
			return
		}
		fmt.Fprintf(w, "%s: %s\n", out.Space, *out.Model)
		if out.NextModel != nil {
			fmt.Fprintf(w, "moving to: %s\nAgents re-embed their memories in the background; engram model --promote finishes the move once the appview has them (getSpaceStatus shows coverage).\n", *out.NextModel)
		}
		if out.DocumentPrefix != "" || out.QueryPrefix != "" {
			fmt.Fprintf(w, "prefixes: memories %q, queries %q\n", out.DocumentPrefix, out.QueryPrefix)
		}
	})
	return nil
}

// cmdIndex lets the appview index a space (or stops it): it opens the
// appview's page, where the authority approves in the browser, and waits
// for the appview to say so.
func (c *cli) cmdIndex(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	sp := fs.String("space", "", "a space you govern, by name or URI (default: the default space)")
	stop := fs.Bool("stop", false, "stop the appview indexing it")
	noWait := fs.Bool("no-wait", false, "print the link and the state, without waiting")
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usageError{fmt.Errorf("unknown argument %q", pos[0])}
	}
	a, _, err := c.agent(ctx)
	if err != nil {
		return err
	}
	out, err := a.IndexSpace(ctx, agent.IndexIn{Space: *sp, Stop: *stop})
	if err != nil {
		return err
	}
	done := func(state string) bool { return (state == "granted") != *stop }
	if out.Link == "" || *noWait || c.json {
		c.print(out, func(w io.Writer) { writeIndex(w, out, *stop) })
		return nil
	}
	fmt.Fprintf(c.err, "Opening the appview's page; sign in as the space's account and approve:\n  %s\n", out.Link)
	if c.browser == nil || c.browser(out.Link) != nil {
		fmt.Fprintln(c.err, "(Open it in a browser.)")
	}
	fmt.Fprintln(c.err, "Waiting for the appview…")
	wait := c.wait
	if wait <= 0 {
		wait = 2 * time.Second
	}
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		if st, err := a.IndexState(ctx, out.Space); err == nil && done(st) {
			out.State, out.Link, out.Note = st, "", ""
			c.print(out, func(w io.Writer) { writeIndex(w, out, *stop) })
			return nil
		}
	}
	return errors.New("gave up waiting for the appview; engram index --no-wait shows the state")
}

func writeIndex(w io.Writer, out agent.IndexOut, stop bool) {
	switch {
	case out.Link != "":
		fmt.Fprintf(w, "%s: the appview's access is %q.\nApprove at: %s\n", out.Space, out.State, out.Link)
	case stop:
		fmt.Fprintf(w, "The appview no longer indexes %s.\n", out.Space)
	default:
		fmt.Fprintf(w, "The appview indexes %s: memories there are searchable.\n", out.Space)
	}
	if out.Note != "" && out.Link == "" {
		fmt.Fprintf(w, "(%s)\n", out.Note)
	}
}
