package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/haileyok/engram-garden/internal/agent"
)

// cmdSpaces lists the spaces (engram spaces), or changes them (engram spaces
// add / remove).
func (c *cli) cmdSpaces(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "add":
			return c.cmdSpacesAdd(args[1:])
		case "remove", "rm":
			return c.cmdSpacesRemove(args[1:])
		case "list", "ls":
			args = args[1:]
		}
	}
	if pos, err := parse(flag.NewFlagSet("spaces", flag.ContinueOnError), args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usageError{fmt.Errorf("unknown argument %q: engram spaces [add <URI> | remove <name>]", pos[0])}
	}
	s, err := c.settings()
	if err != nil {
		return err
	}
	a, err := c.open(ctx, s)
	if err != nil {
		// Without a sign-in, list what's set up, without models.
		out := agent.ListSpacesOut{Spaces: []agent.SpaceInfo{}, Note: "Not signed in, so models aren't shown: " + agent.Explain(err).Error()}
		def, _ := s.Default()
		for _, e := range s.Spaces {
			out.Spaces = append(out.Spaces, agent.SpaceInfo{Name: e.Name, URI: e.URI, SetUp: true, Default: e.URI == def.URI})
		}
		c.print(out, func(w io.Writer) { writeSpaces(w, out) })
		return nil
	}
	out, err := a.ListSpaces(ctx, agent.ListSpacesIn{})
	if err != nil {
		return err
	}
	c.print(out, func(w io.Writer) { writeSpaces(w, out) })
	return nil
}

func writeSpaces(w io.Writer, out agent.ListSpacesOut) {
	var others []agent.SpaceInfo
	setUp := 0
	for _, sp := range out.Spaces {
		if !sp.SetUp {
			others = append(others, sp)
			continue
		}
		setUp++
		mark := "  "
		if sp.Default {
			mark = "* "
		}
		fmt.Fprintf(w, "%s%s\n    %s\n", mark, sp.Name, sp.URI)
		if sp.Model != nil {
			fmt.Fprintf(w, "    model: %s, %d dimensions, digest %s\n", sp.Model.Model, sp.Model.Dims, sp.Model.ModelDigest)
			if sp.NextModel != nil {
				fmt.Fprintf(w, "    moving to: %s, %d dimensions, digest %s\n", sp.NextModel.Model, sp.NextModel.Dims, sp.NextModel.ModelDigest)
			}
			if sp.DocumentPrefix != "" || sp.QueryPrefix != "" {
				fmt.Fprintf(w, "    prefixes: memories %q, queries %q\n", sp.DocumentPrefix, sp.QueryPrefix)
			}
			if sp.LocalModel == "ready" {
				fmt.Fprintln(w, "    this machine: ready")
			} else if sp.LocalModel != "" {
				fmt.Fprintf(w, "    this machine: can't embed with it: %s\n", sp.LocalModel)
			}
		}
		switch {
		case sp.Warning != "":
			fmt.Fprintf(w, "    ✗ indexing: %s\n", sp.Warning)
		case sp.Indexing == "granted":
			fmt.Fprintln(w, "    indexing: the appview can read it")
		}
		if sp.Error != "" {
			fmt.Fprintf(w, "    ✗ %s\n", sp.Error)
		}
	}
	if setUp == 0 {
		fmt.Fprintln(w, "No spaces set up: engram use <space URI>")
	} else {
		fmt.Fprintln(w, "\n* is the default space.")
	}
	if len(others) > 0 {
		fmt.Fprintln(w, "\nThis account also belongs to (engram spaces add <URI> to use one):")
		for _, sp := range others {
			fmt.Fprintf(w, "  %s\n", sp.URI)
		}
	}
	if out.Note != "" {
		fmt.Fprintf(w, "\n(%s)\n", out.Note)
	}
}

// envNote warns when ENGRAM_* variables override the spaces saved here.
func (c *cli) envNote() {
	if c.getenv("ENGRAM_SPACES") != "" || c.getenv("ENGRAM_SPACE") != "" {
		fmt.Fprintln(c.err, "(ENGRAM_SPACES or ENGRAM_SPACE is set, and overrides the saved spaces while it is.)")
	}
}

func (c *cli) cmdSpacesAdd(args []string) error {
	fs := flag.NewFlagSet("spaces add", flag.ContinueOnError)
	name := fs.String("name", "", "what to call it (default: the space's key)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{errors.New("engram spaces add <space URI> [--name name]")}
	}
	s, err := c.fileSettings()
	if err != nil {
		return err
	}
	e, err := s.AddSpace(pos[0], *name)
	if err != nil {
		return usageError{err}
	}
	if err := s.Save(c.configPath); err != nil {
		return err
	}
	def, _ := s.Default()
	c.print(map[string]any{"added": e, "default": def}, func(w io.Writer) {
		fmt.Fprintf(w, "Using %s as %q. Default space: %s.\nCheck it with: engram status\n", e.URI, e.Name, def.Name)
	})
	c.envNote()
	return nil
}

func (c *cli) cmdSpacesRemove(args []string) error {
	pos, err := parse(flag.NewFlagSet("spaces remove", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{errors.New("engram spaces remove <name or URI>")}
	}
	s, err := c.fileSettings()
	if err != nil {
		return err
	}
	if err := s.RemoveSpace(pos[0]); err != nil {
		return usageError{err}
	}
	if err := s.Save(c.configPath); err != nil {
		return err
	}
	def, _ := s.Default()
	c.print(map[string]any{"removed": pos[0], "default": def}, func(w io.Writer) {
		fmt.Fprintf(w, "Stopped using %s.", pos[0])
		if def.Name != "" {
			fmt.Fprintf(w, " Default space: %s.", def.Name)
		}
		fmt.Fprintln(w, " Its memories are untouched.")
	})
	c.envNote()
	return nil
}

func (c *cli) cmdUse(_ context.Context, args []string) error {
	pos, err := parse(flag.NewFlagSet("use", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{errors.New("engram use <space name or URI>")}
	}
	s, err := c.fileSettings()
	if err != nil {
		return err
	}
	e, err := s.UseSpace(pos[0])
	if err != nil {
		return usageError{err}
	}
	if err := s.Save(c.configPath); err != nil {
		return err
	}
	c.print(map[string]any{"default": e}, func(w io.Writer) {
		fmt.Fprintf(w, "Default space: %s (%s)\n", e.Name, e.URI)
	})
	c.envNote()
	return nil
}
