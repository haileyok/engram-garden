package lex

import (
	"strings"
	"testing"
)

func TestValidSkillName(t *testing.T) {
	t.Parallel()
	for name, ok := range map[string]bool{
		"code-review": true, "a": true, "attie-deploy2": true,
		"": false, "Code-Review": false, "-x": false, "x-": false, "a--b": false, "a_b": false, "a.b": false,
		strings.Repeat("a", 64): true, strings.Repeat("a", 65): false,
	} {
		if ValidSkillName(name) != ok {
			t.Errorf("ValidSkillName(%q) = %v, want %v", name, !ok, ok)
		}
	}
}

func TestSkillValidate(t *testing.T) {
	t.Parallel()
	good := func() Skill {
		return Skill{Name: "code-review", Description: "Review code. Use after a change.", Instructions: "# Review\n\nDo it.",
			CreatedAt: "2026-10-21T10:00:00.000Z", UpdatedAt: "2026-10-21T12:00:00+02:00",
			Files: []SkillFile{{Path: "scripts/run.sh", Content: "echo hi", Executable: true}}}
	}
	s := good()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Skill){
		"name":         func(s *Skill) { s.Name = "Bad Name" },
		"description":  func(s *Skill) { s.Description = " " },
		"instructions": func(s *Skill) { s.Instructions = "" },
		"dotdot":       func(s *Skill) { s.Files[0].Path = "../x" },
		"absolute":     func(s *Skill) { s.Files[0].Path = "/etc/passwd" },
		"unclean":      func(s *Skill) { s.Files[0].Path = "a/../b" },
		"backslash":    func(s *Skill) { s.Files[0].Path = `a\b` },
		"skill.md":     func(s *Skill) { s.Files[0].Path = "SKILL.md" },
		"dotfile":      func(s *Skill) { s.Files[0].Path = "scripts/.env" },
		"node_modules": func(s *Skill) { s.Files[0].Path = "node_modules/x.js" },
		"nul":          func(s *Skill) { s.Files[0].Content = "a\x00b" },
		"bad time":     func(s *Skill) { s.UpdatedAt = "Oct 21" },
		"nested skill": func(s *Skill) { s.Files[0].Path = "sub/SKILL.md" },
		"duplicate":    func(s *Skill) { s.Files = append(s.Files, s.Files[0]) },
		"too big": func(s *Skill) {
			s.Files = []SkillFile{{Path: "a", Content: strings.Repeat("x", 190000)}, {Path: "b", Content: strings.Repeat("x", 190000)}, {Path: "c", Content: strings.Repeat("x", 190000)}}
		},
		"long note":     func(s *Skill) { s.Note = strings.Repeat("x", MaxSkillNote+1) },
		"long describe": func(s *Skill) { s.Description = strings.Repeat("x", MaxSkillDescription+1) },
	} {
		s := good()
		mutate(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestSkillRecord(t *testing.T) {
	t.Parallel()
	s := Skill{Name: "x", Description: "d", Instructions: "i", CreatedAt: "c", UpdatedAt: "u"}
	rec := s.Record()
	if rec["$type"] != SkillCollection || rec["files"] != nil || rec["note"] != nil || rec["disableModelInvocation"] != nil {
		t.Fatalf("record %v", rec)
	}
	s.Files = []SkillFile{{Path: "a", Content: "b", Executable: true}}
	s.DisableModelInvocation = true
	rec = s.Record()
	if rec["disableModelInvocation"] != true || len(rec["files"].([]map[string]any)) != 1 {
		t.Fatalf("record %v", rec)
	}
}
