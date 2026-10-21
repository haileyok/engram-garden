package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/haileyok/engram-garden/internal/agent"
	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/skillfile"
)

const skillUsage = "engram skill list | get <name> [--to dir] | put <dir|SKILL.md>... [--note n] | rm <name>   (--space picks a space)"

// cmdSkill manages the skills kept in the spaces.
func (c *cli) cmdSkill(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError{errors.New(skillUsage)}
	}
	switch args[0] {
	case "list", "ls":
		return c.cmdSkillList(ctx, args[1:])
	case "get", "show":
		return c.cmdSkillGet(ctx, args[1:])
	case "put", "save", "import":
		return c.cmdSkillPut(ctx, args[1:])
	case "rm", "delete":
		return c.cmdSkillRm(ctx, args[1:])
	}
	return usageError{fmt.Errorf("unknown skill command %q: %s", args[0], skillUsage)}
}

func (c *cli) cmdSkillList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("skill list", flag.ContinueOnError)
	sp := fs.String("space", "", "only this space")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	a, _, err := c.agent(ctx)
	if err != nil {
		return err
	}
	out, err := a.ListSkills(ctx, agent.ListSkillsIn{Space: *sp})
	if err != nil {
		return err
	}
	me := a.Client.DID().String()
	c.print(out, func(w io.Writer) {
		if len(out.Skills) == 0 {
			fmt.Fprintln(w, "No skills yet. Add one with: engram skill put <directory with a SKILL.md>")
		}
		for _, s := range out.Skills {
			by := ""
			if s.Author != me {
				by = " by " + s.Author
			}
			fmt.Fprintf(w, "%s  (%s%s, updated %s)\n  %s\n", s.Name, s.Space, by, s.UpdatedAt, s.Description)
		}
		if out.Note != "" {
			fmt.Fprintf(w, "Note: %s\n", out.Note)
		}
	})
	return nil
}

func (c *cli) cmdSkillGet(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("skill get", flag.ContinueOnError)
	sp := fs.String("space", "", "the space (default: the first that has it)")
	to := fs.String("to", "", "write the skill's directory here instead of printing it")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{errors.New("engram skill get <name> [--to dir] [--space s]")}
	}
	a, _, err := c.agent(ctx)
	if err != nil {
		return err
	}
	out, err := a.GetSkill(ctx, agent.GetSkillIn{Name: pos[0], Space: *sp})
	if err != nil {
		return err
	}
	if *to != "" {
		if err := skillfile.WriteDir(*to, out.Skill.Skill); err != nil {
			return err
		}
		c.print(map[string]string{"wrote": *to}, func(w io.Writer) { fmt.Fprintf(w, "Wrote %s\n", *to) })
		return nil
	}
	c.print(out, func(w io.Writer) {
		_, _ = w.Write(skillfile.Format(out.Skill.Skill))
		for _, f := range out.Skill.Files {
			fmt.Fprintf(w, "\n----- %s -----\n%s", f.Path, f.Content)
		}
	})
	return nil
}

func (c *cli) cmdSkillPut(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("skill put", flag.ContinueOnError)
	sp := fs.String("space", "", "the space (default: where your version is, else the default space)")
	note := fs.String("note", "", "what this version changes")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usageError{errors.New("engram skill put <skill directory or SKILL.md>... [--space s] [--note n]")}
	}
	a, _, err := c.agent(ctx)
	if err != nil {
		return err
	}
	type saved struct {
		Path string `json:"path"`
		Name string `json:"name"`
		agent.SaveSkillOut
	}
	var all []saved
	for _, p := range pos {
		s, err := skillfile.ReadDir(p)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		files := s.Files
		if files == nil {
			if info, err := os.Stat(p); err == nil && info.IsDir() {
				files = []lex.SkillFile{} // a directory with no files: say so, don't keep the old ones
			}
		}
		dmi := s.DisableModelInvocation
		out, err := a.SaveSkill(ctx, agent.SaveSkillIn{
			Name: s.Name, Description: s.Description, Instructions: s.Instructions, Files: files,
			DisableModelInvocation: &dmi, Note: *note, Space: *sp,
		})
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		all = append(all, saved{Path: p, Name: s.Name, SaveSkillOut: out})
	}
	c.print(all, func(w io.Writer) {
		for _, s := range all {
			verb := "Updated"
			if s.Created {
				verb = "Added"
			}
			fmt.Fprintf(w, "%s %s: %s\n", verb, s.Name, s.URI)
			if s.Note != "" {
				fmt.Fprintf(w, "  Note: %s\n", s.Note)
			}
		}
	})
	return nil
}

func (c *cli) cmdSkillRm(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("skill rm", flag.ContinueOnError)
	sp := fs.String("space", "", "the space (default: where your version is)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{errors.New("engram skill rm <name> [--space s]")}
	}
	a, _, err := c.agent(ctx)
	if err != nil {
		return err
	}
	out, err := a.DeleteSkill(ctx, agent.DeleteSkillIn{Name: pos[0], Space: *sp})
	if err != nil {
		return err
	}
	c.print(out, func(w io.Writer) { fmt.Fprintf(w, "Deleted %s\n", out.Deleted) })
	return nil
}
