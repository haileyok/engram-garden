package skillfile

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/haileyok/engram-garden/internal/lex"
)

const deploy = `---
name: attie-deploy
description: Deploy bluesky-social/attie to pop1 through the deploy repo's Deploy Attie GitHub workflow (every service, one SHA). Use for any Attie deploy, rollback, or "ship it" request.
---

# Deploy Attie

Dispatch the workflow.
`

func TestParse(t *testing.T) {
	t.Parallel()
	s, err := Parse([]byte(deploy))
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "attie-deploy" || !strings.Contains(s.Description, `"ship it"`) || s.Instructions != "# Deploy Attie\n\nDispatch the workflow.\n" {
		t.Fatalf("%+v", s)
	}
	for _, bad := range []string{"no frontmatter", "---\nname: x\n", "---\nname: [\n---\nbody"} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	s, err = Parse([]byte("---\r\nname: x\r\ndescription: d\r\ndisable-model-invocation: true\r\n---"))
	if err != nil || !s.DisableModelInvocation || s.Instructions != "" {
		t.Fatalf("crlf/empty body: %+v %v", s, err)
	}
}

func TestFormatRoundTrips(t *testing.T) {
	t.Parallel()
	for _, s := range []lex.Skill{
		{Name: "a", Description: `Has "quotes": and a colon`, Instructions: "# A\n"},
		{Name: "b", Description: "- starts like a list", Instructions: "body without newline", DisableModelInvocation: true},
		{Name: "c", Description: "multi\nline", Instructions: "x\n"},
	} {
		got, err := Parse(Format(s))
		if err != nil {
			t.Fatal(err)
		}
		want := s
		if !strings.HasSuffix(want.Instructions, "\n") {
			want.Instructions += "\n"
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("round trip:\n got %+v\nwant %+v", got, want)
		}
	}
}

func TestReadWriteDir(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	write := func(rel, content string, mode os.FileMode) {
		p := filepath.Join(src, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("SKILL.md", deploy, 0o644)
	write("scripts/check.sh", "#!/bin/sh\necho ok\n", 0o755)
	write("references/notes.md", "notes", 0o644)
	write(".git/config", "ignored", 0o644)
	write(".DS_Store", "ignored", 0o644)

	s, err := ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Files) != 2 || s.Files[0].Path != "references/notes.md" || s.Files[1].Path != "scripts/check.sh" || !s.Files[1].Executable || s.Files[0].Executable {
		t.Fatalf("files %+v", s.Files)
	}
	s.CreatedAt, s.UpdatedAt = "2026-10-21T00:00:00.000Z", "2026-10-21T00:00:00.000Z"

	dst := filepath.Join(t.TempDir(), "attie-deploy")
	write2 := filepath.Join(dst, "stale.txt")
	_ = os.MkdirAll(dst, 0o755)
	_ = os.WriteFile(write2, []byte("old"), 0o644)
	_ = os.WriteFile(filepath.Join(dst, "SKILL.md"), []byte(deploy), 0o644) // an older version of the skill
	if err := WriteDir(dst, s); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(write2); !os.IsNotExist(err) {
		t.Fatal("stale file kept")
	}
	back, err := ReadDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	back.CreatedAt, back.UpdatedAt = s.CreatedAt, s.UpdatedAt
	if !reflect.DeepEqual(back, s) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", back, s)
	}
	if fi, _ := os.Stat(filepath.Join(dst, "scripts/check.sh")); fi.Mode()&0o111 == 0 {
		t.Fatal("not executable")
	}

	// A directory that isn't a skill is never replaced.
	project := t.TempDir()
	_ = os.WriteFile(filepath.Join(project, "main.go"), []byte("package main"), 0o644)
	if err := WriteDir(project, s); err == nil || !strings.Contains(err.Error(), "isn't a skill directory") {
		t.Fatalf("replacing a project: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, "main.go")); err != nil {
		t.Fatal("project file gone")
	}
	// Nor is a file.
	file := filepath.Join(t.TempDir(), "notes.txt")
	_ = os.WriteFile(file, []byte("keep me"), 0o644)
	if err := WriteDir(file, s); err == nil || !strings.Contains(err.Error(), "isn't a directory") {
		t.Fatalf("replacing a file: %v", err)
	}
	if b, _ := os.ReadFile(file); string(b) != "keep me" {
		t.Fatal("file changed")
	}

	// An empty one is fine, and so is one whose parents don't exist yet.
	if err := WriteDir(t.TempDir(), s); err != nil {
		t.Fatal(err)
	}
	if err := WriteDir(filepath.Join(t.TempDir(), "a", "b", "c"), s); err != nil {
		t.Fatal(err)
	}

	// Binary files can't be part of a skill.
	write("img.png", "\x89PNG\x00\x01", 0o644)
	if _, err := ReadDir(src); err == nil || !strings.Contains(err.Error(), "isn't text") {
		t.Fatalf("binary: %v", err)
	}
	// A lone SKILL.md works too.
	one, err := ReadDir(filepath.Join(src, "SKILL.md"))
	if err != nil || one.Name != "attie-deploy" || len(one.Files) != 0 {
		t.Fatalf("lone file: %+v %v", one, err)
	}
}

// TestParseLikePi: the frontmatter cases pi/test/skills.test.ts checks
// parse the same way here.
func TestParseLikePi(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"---\nname: attie-deploy\ndescription: Deploy Attie to pop1,\n  which is the whole point\n  of this skill.\nlicense: \"MIT\" # a comment\n---\nx\n": "Deploy Attie to pop1, which is the whole point of this skill.",
		"---\nname: x\ndescription: 'it''s' # c\n---\n":     "it's",
		"---\nname: x\ndescription: >\n  one\n  two\n---\n": "one two",
		"---\nname: x\ndescription: |\n  one\n  two\n---\n": "one\ntwo",
	} {
		s, err := Parse([]byte(in))
		if err != nil || s.Description != want {
			t.Errorf("Parse(%q) description = %q, %v; want %q", in, s.Description, err, want)
		}
	}
}

// TestParseCloseLikePi: the closing line must be exactly ---.
func TestParseCloseLikePi(t *testing.T) {
	t.Parallel()
	if _, err := Parse([]byte("---\nname: x\n--- \ndescription: d\n")); err == nil {
		t.Error("a --- with a trailing space closed the frontmatter")
	}
	s, err := Parse([]byte("---\nname: x\n----: v\ndescription: d\n---\nbody\n"))
	if err != nil || s.Description != "d" || s.Instructions != "body\n" {
		t.Errorf("%+v %v", s, err)
	}
}

// TestParseFoldedLikePi: a blank line in a folded value is a line break,
// and a frontmatter that closes on its first line isn't one.
func TestParseFoldedLikePi(t *testing.T) {
	t.Parallel()
	s, err := Parse([]byte("---\nname: x\ndescription: >\n  one\n  two\n\n  three\n---\n"))
	if err != nil || s.Description != "one two\nthree" {
		t.Errorf("folded: %q %v", s.Description, err)
	}
	if _, err := Parse([]byte("---\n---\nbody")); err == nil {
		t.Error("empty frontmatter parsed")
	}
}
