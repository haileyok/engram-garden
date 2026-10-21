// Package skillfile converts between skill records and skill directories in
// the Agent Skills format: a SKILL.md (YAML frontmatter, then Markdown
// instructions) and the files beside it.
package skillfile

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/haileyok/engram-garden/internal/lex"
)

// frontmatter is the part of SKILL.md's frontmatter a skill record keeps.
// Other keys (license, metadata, ...) aren't kept.
type frontmatter struct {
	Name                   string `yaml:"name"`
	Description            string `yaml:"description"`
	DisableModelInvocation bool   `yaml:"disable-model-invocation,omitempty"`
}

// Parse reads a SKILL.md into a skill's name, description, invocation
// setting and instructions.
func Parse(data []byte) (lex.Skill, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return lex.Skill{}, errors.New("SKILL.md must start with YAML frontmatter between --- lines")
	}
	head, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		if h, found := strings.CutSuffix(rest, "\n---"); found {
			head, body, ok = h, "", true
		}
	}
	if !ok {
		return lex.Skill{}, errors.New("SKILL.md's frontmatter has no closing --- line")
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(head), &fm); err != nil {
		return lex.Skill{}, fmt.Errorf("SKILL.md frontmatter: %w", err)
	}
	return lex.Skill{
		Name:                   strings.TrimSpace(fm.Name),
		Description:            strings.TrimSpace(fm.Description),
		DisableModelInvocation: fm.DisableModelInvocation,
		Instructions:           strings.TrimLeft(body, "\n"),
	}, nil
}

// Format writes a skill's SKILL.md.
func Format(s lex.Skill) []byte {
	head, err := yaml.Marshal(frontmatter{Name: s.Name, Description: s.Description, DisableModelInvocation: s.DisableModelInvocation})
	if err != nil {
		panic(err) // three plain fields always marshal
	}
	var b bytes.Buffer
	b.WriteString("---\n")
	b.Write(head)
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimLeft(s.Instructions, "\n"))
	if !strings.HasSuffix(s.Instructions, "\n") {
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// skipped are names a skill directory's files never include.
func skipped(name string) bool {
	return strings.HasPrefix(name, ".") || name == "node_modules" || name == "__pycache__"
}

// ReadDir reads a skill directory, or a lone SKILL.md file.
func ReadDir(path string) (lex.Skill, error) {
	info, err := os.Stat(path)
	if err != nil {
		return lex.Skill{}, err
	}
	dir := path
	if !info.IsDir() {
		dir = filepath.Dir(path)
		data, err := os.ReadFile(path)
		if err != nil {
			return lex.Skill{}, err
		}
		return Parse(data)
	}
	data, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return lex.Skill{}, fmt.Errorf("%s has no SKILL.md", dir)
	}
	s, err := Parse(data)
	if err != nil {
		return s, err
	}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		if skipped(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "SKILL.md" {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: only regular files can be part of a skill", rel)
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
			return fmt.Errorf("%s isn't text; a skill's files are kept as text", rel)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		s.Files = append(s.Files, lex.SkillFile{Path: rel, Content: string(content), Executable: fi.Mode()&0o111 != 0})
		return nil
	})
	slices.SortFunc(s.Files, func(a, b lex.SkillFile) int { return strings.Compare(a.Path, b.Path) })
	return s, err
}

// WriteDir writes a skill into dir (SKILL.md and its files), replacing
// what was there. It refuses to replace a directory that isn't empty and
// isn't a skill (has no SKILL.md), and puts the old directory back if the
// swap fails.
func WriteDir(dir string, s lex.Skill) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if fi, err := os.Stat(dir); err == nil && !fi.IsDir() {
		return fmt.Errorf("%s isn't a directory; not replacing it", dir)
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
			return fmt.Errorf("%s isn't empty and isn't a skill directory (no SKILL.md); not replacing it", dir)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), "."+filepath.Base(dir)+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := os.WriteFile(filepath.Join(tmp, "SKILL.md"), Format(s), 0o644); err != nil {
		return err
	}
	for _, f := range s.Files {
		p := filepath.Join(tmp, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if f.Executable {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(f.Content), mode); err != nil {
			return err
		}
	}
	old := ""
	if _, err := os.Stat(dir); err == nil {
		old = tmp + ".old"
		if err := os.Rename(dir, old); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		if old != "" {
			_ = os.Rename(old, dir)
		}
		return err
	}
	if old != "" {
		return os.RemoveAll(old)
	}
	return nil
}
