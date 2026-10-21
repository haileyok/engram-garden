package lex

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

const (
	// SkillCollection holds skills, keyed by name.
	SkillCollection = "garden.engram.skill"
	// SessionCollection holds where agents' working sessions stand, keyed
	// by the harness's session ID.
	SessionCollection = "garden.engram.session"
)

// Skill is a garden.engram.skill record: instructions for a kind of work in
// the Agent Skills format, a SKILL.md and the files beside it.
type Skill struct {
	Name                   string      `json:"name"`
	Description            string      `json:"description"`
	Instructions           string      `json:"instructions"`
	Files                  []SkillFile `json:"files,omitempty"`
	DisableModelInvocation bool        `json:"disableModelInvocation,omitempty"`
	Note                   string      `json:"note,omitempty"`
	CreatedAt              string      `json:"createdAt"`
	UpdatedAt              string      `json:"updatedAt"`
}

// SkillFile is a file beside SKILL.md.
type SkillFile struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	Executable bool   `json:"executable,omitempty"`
}

// Limits on a skill, from the lexicon. MaxSkillBytes keeps the whole record
// well inside what a PDS accepts.
const (
	MaxSkillName         = 64
	MaxSkillDescription  = 1024
	MaxSkillInstructions = 200000
	MaxSkillFiles        = 32
	MaxSkillFilePath     = 512
	MaxSkillFileContent  = 200000
	MaxSkillNote         = 2000
	MaxSkillBytes        = 500000
)

var skillName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidSkillName reports whether name is a valid skill name: lowercase
// letters, digits and single hyphens between them, at most 64 characters.
// Such a name is also a valid record key.
func ValidSkillName(name string) bool {
	return len(name) <= MaxSkillName && skillName.MatchString(name)
}

// Validate checks a skill against the lexicon's rules.
func (s *Skill) Validate() error {
	if !ValidSkillName(s.Name) {
		return fmt.Errorf("skill name %q: use lowercase letters, digits and single hyphens (at most %d characters)", s.Name, MaxSkillName)
	}
	if strings.TrimSpace(s.Description) == "" {
		return errors.New("a skill needs a description: what it does and when to use it")
	}
	if len(s.Description) > MaxSkillDescription {
		return fmt.Errorf("description is too long (at most %d bytes)", MaxSkillDescription)
	}
	if strings.TrimSpace(s.Instructions) == "" {
		return errors.New("a skill needs instructions")
	}
	if len(s.Instructions) > MaxSkillInstructions {
		return fmt.Errorf("instructions are too long (at most %d bytes)", MaxSkillInstructions)
	}
	if len(s.Note) > MaxSkillNote {
		return fmt.Errorf("note is too long (at most %d bytes)", MaxSkillNote)
	}
	for _, t := range []struct{ name, v string }{{"createdAt", s.CreatedAt}, {"updatedAt", s.UpdatedAt}} {
		if _, err := time.Parse(time.RFC3339Nano, t.v); err != nil {
			return fmt.Errorf("%s %q isn't an RFC 3339 time", t.name, t.v)
		}
	}
	if len(s.Files) > MaxSkillFiles {
		return fmt.Errorf("at most %d files", MaxSkillFiles)
	}
	total := len(s.Description) + len(s.Instructions) + len(s.Note)
	seen := map[string]bool{}
	for _, f := range s.Files {
		if err := ValidSkillFilePath(f.Path); err != nil {
			return err
		}
		if seen[f.Path] {
			return fmt.Errorf("file %s is listed twice", f.Path)
		}
		seen[f.Path] = true
		if strings.ContainsRune(f.Content, 0) {
			return fmt.Errorf("file %s isn't text", f.Path)
		}
		if len(f.Content) > MaxSkillFileContent {
			return fmt.Errorf("file %s is too long (at most %d bytes)", f.Path, MaxSkillFileContent)
		}
		total += len(f.Path) + len(f.Content)
	}
	if total > MaxSkillBytes {
		return fmt.Errorf("the skill is too big (%d bytes; at most %d in all)", total, MaxSkillBytes)
	}
	return nil
}

// ValidSkillFilePath checks a skill file's path: relative, forward slashes,
// inside the skill's directory, and not SKILL.md itself.
func ValidSkillFilePath(p string) error {
	switch {
	case p == "" || len(p) > MaxSkillFilePath:
		return fmt.Errorf("file path %q: empty or too long", p)
	case strings.Contains(p, "\\") || strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0):
		return fmt.Errorf("file path %q: use a relative path with forward slashes", p)
	case path.Clean(p) != p:
		return fmt.Errorf("file path %q: not in its simplest form", p)
	case p == ".." || strings.HasPrefix(p, "../"):
		return fmt.Errorf("file path %q: outside the skill's directory", p)
	case p == "SKILL.md":
		return errors.New("SKILL.md is the skill's instructions, not one of its files")
	}
	// Names a skill directory never picks up when read back (dotfiles,
	// installed dependencies), so a record never holds a file that editing
	// the directory would silently drop.
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, ".") || seg == "node_modules" || seg == "__pycache__" {
			return fmt.Errorf("file path %q: dotfiles, node_modules and __pycache__ aren't kept in skills", p)
		}
		// Agents find skills by their SKILL.md, at any depth.
		if seg == "SKILL.md" {
			return fmt.Errorf("file path %q: a SKILL.md inside a skill would be a skill of its own", p)
		}
	}
	return nil
}

// Record is the skill as a record to write, with its $type.
func (s *Skill) Record() map[string]any {
	rec := map[string]any{
		"$type":        SkillCollection,
		"name":         s.Name,
		"description":  s.Description,
		"instructions": s.Instructions,
		"createdAt":    s.CreatedAt,
		"updatedAt":    s.UpdatedAt,
	}
	if len(s.Files) > 0 {
		files := make([]map[string]any, len(s.Files))
		for i, f := range s.Files {
			m := map[string]any{"path": f.Path, "content": f.Content}
			if f.Executable {
				m["executable"] = true
			}
			files[i] = m
		}
		rec["files"] = files
	}
	if s.DisableModelInvocation {
		rec["disableModelInvocation"] = true
	}
	if s.Note != "" {
		rec["note"] = s.Note
	}
	return rec
}
