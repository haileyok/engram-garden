package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/haileyok/cocoon/space"

	"github.com/haileyok/engram-garden/internal/lex"
	"github.com/haileyok/engram-garden/internal/spaceclient"
)

// Skills live in members' own repos in a space, keyed by name. They aren't
// indexed: an agent reads every member's skills directly from their PDS
// with the space credential. Several members may keep a skill with the same
// name; for each reader, its own wins, then the authority's, then the most
// recently updated.

// SkillInfo is a skill as an agent sees it: the record, where it is, and
// which version of the name it is.
type SkillInfo struct {
	lex.Skill
	// Space names the space the skill is in.
	Space  string `json:"space,omitempty"`
	URI    string `json:"uri"`
	CID    string `json:"cid"`
	Author string `json:"author"`
	// Shadowed lists other versions of the name in the space, by URI, that
	// this one wins over.
	Shadowed []string `json:"shadowed,omitempty"`
}

type ListSkillsIn struct {
	Space string `json:"space,omitempty" jsonschema:"only this space, by name or URI (default: every space you use)"`
	// Content includes each skill's instructions and files, for an agent
	// that keeps them on disk; without it only names and descriptions come
	// back.
	Content bool `json:"content,omitempty" jsonschema:"include each skill's instructions and files, not only its name and description"`
}

type ListSkillsOut struct {
	Skills []SkillInfo `json:"skills"`
	// Note explains spaces that couldn't be read.
	Note string `json:"note,omitempty"`
}

type GetSkillIn struct {
	Name  string `json:"name" jsonschema:"the skill's name"`
	Space string `json:"space,omitempty" jsonschema:"the space, by name or URI (default: the first space you use that has it)"`
}

type GetSkillOut struct {
	Skill SkillInfo `json:"skill"`
}

type SaveSkillIn struct {
	Name                   string          `json:"name" jsonschema:"lowercase letters, digits and hyphens, such as code-review"`
	Description            string          `json:"description" jsonschema:"what the skill does and when to use it; agents see only this until they load it"`
	Instructions           string          `json:"instructions" jsonschema:"the skill's instructions in Markdown (the body of SKILL.md, without frontmatter): the whole new text, not a diff"`
	Files                  []lex.SkillFile `json:"files,omitempty" jsonschema:"files beside SKILL.md (scripts, references), as text; omit to keep the current files"`
	DisableModelInvocation *bool           `json:"disableModelInvocation,omitempty" jsonschema:"load the skill only when a person asks for it by name"`
	Note                   string          `json:"note,omitempty" jsonschema:"what this version changes and why, such as 'step 3 used the wrong workflow name'"`
	Space                  string          `json:"space,omitempty" jsonschema:"the space, by name or URI (default: where your version of the skill is, else the default space)"`
}

type SaveSkillOut struct {
	URI     string `json:"uri"`
	CID     string `json:"cid"`
	Created bool   `json:"created"`
	// Space names the space it was saved in.
	Space string `json:"space,omitempty"`
	// Note says what to know, such as that another member's version of the
	// name exists.
	Note string `json:"note,omitempty"`
}

type DeleteSkillIn struct {
	Name  string `json:"name" jsonschema:"the skill's name"`
	Space string `json:"space,omitempty" jsonschema:"the space, by name or URI (default: where your version of the skill is)"`
}

type DeleteSkillOut struct {
	Deleted string `json:"deleted"`
}

// ---- one space ----

// skillRecords reads every member's skills in the space, and says whose
// couldn't be read.
func (t *Agent) skillRecords(ctx context.Context) ([]SkillInfo, []string, error) {
	members, err := t.members(ctx)
	if err != nil {
		return nil, nil, err
	}
	me := t.Client.DID().String()
	if !slices.Contains(members, me) {
		members = append(members, me)
	}
	var (
		mu         sync.Mutex
		all        []SkillInfo
		errs       []error
		unreadable []string
		wg         sync.WaitGroup
	)
	for _, did := range members {
		wg.Go(func() {
			recs, err := t.listRecords(ctx, did, lex.SkillCollection)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", did, err))
				unreadable = append(unreadable, did)
				return
			}
			for _, r := range recs {
				var s lex.Skill
				if json.Unmarshal(r.Value, &s) != nil || s.Name != r.Rkey || s.Validate() != nil {
					continue // not a skill this agent can use
				}
				all = append(all, SkillInfo{Skill: s, URI: r.URI, CID: r.CID, Author: did})
			}
		})
	}
	wg.Wait()
	if len(errs) > 0 && len(all) == 0 {
		return nil, nil, errors.Join(errs...)
	}
	slices.Sort(unreadable)
	return all, unreadable, nil
}

// resolveSkills keeps one version of each name: the reader's own, then the
// authority's, then the most recently updated.
func resolveSkills(all []SkillInfo, me, authority string) []SkillInfo {
	rank := func(s SkillInfo) int {
		switch s.Author {
		case me:
			return 0
		case authority:
			return 1
		}
		return 2
	}
	// Newest by instant, not by how the time is written; a time in the
	// future (a clock off, or a member trying to win) counts as oldest.
	limit := time.Now().Add(time.Hour)
	when := func(s SkillInfo) time.Time {
		t, err := time.Parse(time.RFC3339Nano, s.UpdatedAt)
		if err != nil || t.After(limit) {
			return time.Time{}
		}
		return t
	}
	byName := map[string][]SkillInfo{}
	for _, s := range all {
		byName[s.Name] = append(byName[s.Name], s)
	}
	out := make([]SkillInfo, 0, len(byName))
	for _, versions := range byName {
		slices.SortFunc(versions, func(a, b SkillInfo) int {
			if c := cmp.Compare(rank(a), rank(b)); c != 0 {
				return c
			}
			if c := when(b).Compare(when(a)); c != 0 {
				return c
			}
			return cmp.Compare(a.URI, b.URI) // the same choice every time, in every client
		})
		win := versions[0]
		for _, v := range versions[1:] {
			win.Shadowed = append(win.Shadowed, v.URI)
		}
		out = append(out, win)
	}
	slices.SortFunc(out, func(a, b SkillInfo) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// Skills lists the skills in the space, one version per name, and the
// members whose skills couldn't be read (so the list may lack a version).
func (t *Agent) Skills(ctx context.Context) ([]SkillInfo, []string, error) {
	all, unreadable, err := t.skillRecords(ctx)
	if err != nil {
		return nil, nil, err
	}
	ref, err := space.ParseRef(t.Space)
	if err != nil {
		return nil, nil, err
	}
	return resolveSkills(all, t.Client.DID().String(), ref.Authority), unreadable, nil
}

// ownSkill reads the agent's own version of a skill, if it has one.
func (t *Agent) ownSkill(ctx context.Context, name string) (*lex.Skill, error) {
	params := map[string]any{"space": t.Space, "repo": t.Client.DID().String(), "collection": lex.SkillCollection, "rkey": name}
	var out struct {
		Value json.RawMessage `json:"value"`
	}
	if err := t.Client.Session.Get(ctx, "com.atproto.space.getRecord", params, &out); err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading your version of %s: %w", name, err)
	}
	var s lex.Skill
	if err := json.Unmarshal(out.Value, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func isNotFound(err error) bool {
	if spaceclient.IsError(err, "RecordNotFound", "RepoNotFound", "NotFound") {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "RecordNotFound") || strings.Contains(msg, "RepoNotFound") || strings.Contains(msg, "Could not locate record")
}

// SaveSkill writes the agent's own version of a skill, keeping the current
// version's files and createdAt when it has one and the input leaves them
// out.
func (t *Agent) SaveSkill(ctx context.Context, in SaveSkillIn) (SaveSkillOut, error) {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	s := lex.Skill{
		Name:         strings.TrimSpace(in.Name),
		Description:  strings.TrimSpace(in.Description),
		Instructions: strings.TrimSpace(in.Instructions) + "\n",
		Files:        in.Files,
		Note:         strings.TrimSpace(in.Note),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if !lex.ValidSkillName(s.Name) {
		return SaveSkillOut{}, (&s).Validate()
	}
	cur, err := t.ownSkill(ctx, s.Name)
	if err != nil {
		return SaveSkillOut{}, err
	}
	if cur != nil {
		if cur.CreatedAt != "" {
			s.CreatedAt = cur.CreatedAt
		}
		if in.Files == nil {
			s.Files = cur.Files
		}
		s.DisableModelInvocation = cur.DisableModelInvocation
	}
	if in.DisableModelInvocation != nil {
		s.DisableModelInvocation = *in.DisableModelInvocation
	}
	if err := s.Validate(); err != nil {
		return SaveSkillOut{}, err
	}
	var out SaveSkillOut
	body := map[string]any{"space": t.Space, "repo": t.Client.DID().String(), "collection": lex.SkillCollection, "rkey": s.Name, "record": s.Record()}
	if err := t.Client.Session.Post(ctx, "com.atproto.space.putRecord", body, &out); err != nil {
		return SaveSkillOut{}, fmt.Errorf("saving skill %s: %w", s.Name, scopeHint(err, "skills"))
	}
	out.Created = cur == nil
	return out, nil
}

// DeleteSkill deletes the agent's own version of a skill.
func (t *Agent) DeleteSkill(ctx context.Context, name string) (DeleteSkillOut, error) {
	if !lex.ValidSkillName(name) {
		return DeleteSkillOut{}, fmt.Errorf("%q isn't a skill name", name)
	}
	cur, err := t.ownSkill(ctx, name)
	if err != nil {
		return DeleteSkillOut{}, err
	}
	if cur == nil {
		return DeleteSkillOut{}, fmt.Errorf("you have no skill %s in this space; you can only delete your own", name)
	}
	ref, err := space.ParseRef(t.Space)
	if err != nil {
		return DeleteSkillOut{}, err
	}
	me := t.Client.DID().String()
	body := map[string]any{"space": t.Space, "repo": me, "collection": lex.SkillCollection, "rkey": name}
	if err := t.Client.Session.Post(ctx, "com.atproto.space.deleteRecord", body, nil); err != nil {
		return DeleteSkillOut{}, fmt.Errorf("deleting skill %s: %w", name, scopeHint(err, "skills"))
	}
	return DeleteSkillOut{Deleted: ref.RecordURI(me, lex.SkillCollection, name)}, nil
}

// scopeHint explains a refused write to a collection an older sign-in
// didn't ask for.
func scopeHint(err error, what string) error {
	var xe *spaceclient.Error
	msg := err.Error()
	refused := (errors.As(err, &xe) && (xe.Status == 401 || xe.Status == 403)) ||
		strings.Contains(msg, "scope") || strings.Contains(msg, "Forbidden")
	if !refused {
		return err
	}
	return fmt.Errorf("%w (if your sign-in predates %s, run `engram login` again to allow writing them)", err, what)
}

// ---- reading members' repos ----

type spaceRecord struct {
	URI   string
	CID   string
	Rkey  string
	Value json.RawMessage
}

// members lists the DIDs with a repo in the space, from its authority.
func (t *Agent) members(ctx context.Context) ([]string, error) {
	ref, err := space.ParseRef(t.Space)
	if err != nil {
		return nil, err
	}
	host, err := t.Client.SpaceHost(ctx, ref.Authority)
	if err != nil {
		return nil, err
	}
	var dids []string
	cursor := ""
	for {
		params := url.Values{"space": {t.Space}, "limit": {"100"}}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var out struct {
			Repos []struct {
				DID string `json:"did"`
			} `json:"repos"`
			Cursor string `json:"cursor"`
		}
		if err := t.Client.Query(ctx, host, t.Space, ref.Authority, "com.atproto.space.listRepos", params, &out); err != nil {
			return nil, fmt.Errorf("listing the space's members: %w", err)
		}
		for _, r := range out.Repos {
			dids = append(dids, r.DID)
		}
		if out.Cursor == "" || len(out.Repos) == 0 {
			return dids, nil
		}
		cursor = out.Cursor
	}
}

// listRecords reads a member's records in a collection from its PDS.
func (t *Agent) listRecords(ctx context.Context, did, collection string) ([]spaceRecord, error) {
	ref, err := space.ParseRef(t.Space)
	if err != nil {
		return nil, err
	}
	host, err := t.Client.PDSHost(ctx, did)
	if err != nil {
		return nil, err
	}
	var recs []spaceRecord
	cursor := ""
	for {
		params := url.Values{"space": {t.Space}, "repo": {did}, "collection": {collection}, "limit": {"100"}}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var out struct {
			Records []struct {
				CID        string          `json:"cid"`
				Collection string          `json:"collection"`
				Rkey       string          `json:"rkey"`
				Value      json.RawMessage `json:"value"`
			} `json:"records"`
			Cursor string `json:"cursor"`
		}
		if err := t.Client.Query(ctx, host, t.Space, did, "com.atproto.space.listRecords", params, &out); err != nil {
			if isNotFound(err) {
				return recs, nil
			}
			return nil, err
		}
		for _, r := range out.Records {
			if r.Collection != "" && r.Collection != collection {
				continue
			}
			recs = append(recs, spaceRecord{URI: ref.RecordURI(did, collection, r.Rkey), CID: r.CID, Rkey: r.Rkey, Value: r.Value})
		}
		if out.Cursor == "" || len(out.Records) == 0 {
			return recs, nil
		}
		cursor = out.Cursor
	}
}

// ---- every space ----

// ListSkills lists the skills in every space the agent uses (or one), one
// version per name in each space.
func (s *Spaces) ListSkills(ctx context.Context, in ListSkillsIn) (ListSkillsOut, error) {
	entries := s.settings().Spaces
	if in.Space != "" {
		e, err := s.settings().Resolve(in.Space)
		if err != nil {
			return ListSkillsOut{}, err
		}
		entries = []SpaceEntry{e}
	}
	results := make([][]SkillInfo, len(entries))
	unreadable := make([][]string, len(entries))
	errs := make([]error, len(entries))
	var wg sync.WaitGroup
	for i, e := range entries {
		wg.Go(func() {
			a, _, err := s.Agent(e.URI)
			if err == nil {
				results[i], unreadable[i], err = a.Skills(ctx)
			}
			errs[i] = err
		})
	}
	wg.Wait()
	out := ListSkillsOut{Skills: []SkillInfo{}}
	var notes []string
	failed := 0
	for i, e := range entries {
		if errs[i] != nil {
			notes = append(notes, fmt.Sprintf("couldn't read skills in %s: %v", e.Name, unwrap(errs[i])))
			failed++
			continue
		}
		if len(unreadable[i]) > 0 {
			notes = append(notes, fmt.Sprintf("couldn't read %s's skills in %s, so their versions are missing", strings.Join(unreadable[i], ", "), e.Name))
		}
		for _, sk := range results[i] {
			sk.Space = e.Name
			if !in.Content {
				sk.Instructions = ""
				sk.Files = nil
			}
			out.Skills = append(out.Skills, sk)
		}
	}
	if failed == len(entries) && len(entries) > 0 {
		return out, errors.New(strings.Join(notes, "; "))
	}
	out.Note = strings.Join(notes, "; ")
	return out, nil
}

// GetSkill returns one skill, in full.
func (s *Spaces) GetSkill(ctx context.Context, in GetSkillIn) (GetSkillOut, error) {
	all, err := s.ListSkills(ctx, ListSkillsIn{Space: in.Space, Content: true})
	if err != nil {
		return GetSkillOut{}, err
	}
	for _, sk := range all.Skills {
		if sk.Name == in.Name {
			return GetSkillOut{Skill: sk}, nil
		}
	}
	return GetSkillOut{}, fmt.Errorf("no skill named %s%s", in.Name, inSpace(in.Space))
}

func inSpace(name string) string {
	if name == "" {
		return ""
	}
	return " in " + name
}

// SaveSkill writes the agent's own version of a skill. Without a space, it
// goes where the agent's version already is, else to the default space.
func (s *Spaces) SaveSkill(ctx context.Context, in SaveSkillIn) (SaveSkillOut, error) {
	target := in.Space
	var others []SkillInfo
	if all, err := s.ListSkills(ctx, ListSkillsIn{Space: in.Space}); err == nil {
		me := s.Client.DID().String()
		for _, sk := range all.Skills {
			if sk.Name != in.Name {
				continue
			}
			if target == "" && sk.Author == me {
				target = sk.Space
			}
			if sk.Author != me {
				others = append(others, sk)
			}
		}
	}
	a, e, err := s.Agent(target)
	if err != nil {
		return SaveSkillOut{}, err
	}
	out, err := a.SaveSkill(ctx, in)
	if err != nil {
		return out, err
	}
	out.Space = e.Name
	for _, o := range others {
		if o.Space == e.Name {
			out.Note = fmt.Sprintf("%s also keeps a skill named %s here; yours is what you'll use, theirs is what they use", o.Author, in.Name)
		}
	}
	return out, nil
}

// DeleteSkill deletes the agent's own version of a skill.
func (s *Spaces) DeleteSkill(ctx context.Context, in DeleteSkillIn) (DeleteSkillOut, error) {
	target := in.Space
	if target == "" {
		all, err := s.ListSkills(ctx, ListSkillsIn{})
		if err != nil {
			return DeleteSkillOut{}, err
		}
		me := s.Client.DID().String()
		for _, sk := range all.Skills {
			if sk.Name == in.Name && sk.Author == me {
				target = sk.Space
				break
			}
		}
		if target == "" {
			return DeleteSkillOut{}, fmt.Errorf("you have no skill named %s; you can only delete your own", in.Name)
		}
	}
	a, _, err := s.Agent(target)
	if err != nil {
		return DeleteSkillOut{}, err
	}
	return a.DeleteSkill(ctx, in.Name)
}
