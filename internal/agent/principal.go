package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/haileyok/cocoon/space"

	"github.com/haileyok/engram-garden/internal/lex"
)

// A principal is the person agents act for (https://ident.link). Engram can
// give every agent session its own ATProto account: the principal's own PDS
// creates it with createAgentNSID, called with the principal's session, which
// puts the ident.link agent-of declaration in the account's first PLC
// operation and the principal's endorsement in their repo, in one request.
// Engram then adds the agent to the principal's lounge. The principal's
// settings live in principal.json beside config.json; the agents' identities
// in agents/. See docs/design/lounge.md.

// CreateAgentNSID is the PDS method that creates an agent account for the
// caller (Cocoon's at.hailey.cocoon.createAgent).
const CreateAgentNSID = "at.hailey.cocoon.createAgent"

// PrincipalScopes are what the principal's sign-in asks for: create agents
// at their PDS (whose DID is pdsDID), endorse them (link.ident.endorsement
// records in their own repo), and create lounges and manage who's in them.
func PrincipalScopes(pdsDID string) []string {
	return []string{
		"atproto",
		"rpc:" + CreateAgentNSID + "?aud=" + pdsDID,
		"repo:" + lex.EndorsementCollection,
		"space:" + lex.LoungeType + "?collection=" + lex.PresenceCollection + "&action=read&manage=create&manage=update",
		// The archive: create it, manage its members, declare its model,
		// and read every session in it.
		"space:" + lex.ArchiveType + "?collection=" + lex.SessionCollection + "&collection=" + lex.ConfigCollection + "&action=read&action=create&action=update&manage=create&manage=update",
	}
}

// PDSDID asks a PDS for its DID (describeServer).
func PDSDID(ctx context.Context, host string, httpc *http.Client) (string, error) {
	api := atclient.NewAPIClient(strings.TrimSuffix(host, "/"))
	if httpc != nil {
		api.Client = httpc
	}
	var out struct {
		DID string `json:"did"`
	}
	if err := api.Get(ctx, "com.atproto.server.describeServer", nil, &out); err != nil {
		return "", fmt.Errorf("asking %s for its DID: %w", host, err)
	}
	if _, err := syntax.ParseDID(out.DID); err != nil {
		return "", fmt.Errorf("%s gave a bad DID %q", host, out.DID)
	}
	return out.DID, nil
}

// Principal configures creating agent identities.
type Principal struct {
	// Account is the principal's own sign-in (OAuth), and Scopes what it
	// was granted with: resuming it needs the same ones.
	Account Account  `json:"account"`
	Scopes  []string `json:"scopes,omitempty"`
	// RecoveryKey is a did:key the principal holds offline. It goes first in
	// every agent's rotation keys, so the principal can undo any change to an
	// agent's identity. Only its public half is here.
	RecoveryKey string `json:"recoveryKey,omitempty"`
	// Lounge is the lounge every new agent joins.
	Lounge string `json:"lounge,omitempty"`
	// Archive is the archive every new agent keeps its sessions in.
	Archive string `json:"archive,omitempty"`
	// Stock is how many unclaimed identities to keep ready, so a session
	// doesn't wait for its account to be created (default 4).
	Stock int `json:"stock,omitempty"`
}

// PrincipalPath is principal.json in ConfigDir.
func PrincipalPath() (string, error) {
	d, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "principal.json"), nil
}

// LoadPrincipal reads principal.json. A missing file is an error saying how
// to set it up.
func LoadPrincipal(path string) (Principal, error) {
	var p Principal
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return p, fmt.Errorf("no principal is set up (%s): run `engram principal login`", path)
	}
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// Save writes principal.json, readable only by its owner.
func (p Principal) Save(path string) error {
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Check reports what's missing before agents can be created.
func (p Principal) Check() error {
	if p.Account.DID == "" {
		return errors.New("no principal is signed in: run `engram principal login`")
	}
	if p.Lounge != "" {
		ref, err := space.ParseRef(p.Lounge)
		if err != nil || ref.Type != lex.LoungeType {
			return fmt.Errorf("lounge must be a %s space URI, not %q", lex.LoungeType, p.Lounge)
		}
	}
	if p.Archive != "" {
		ref, err := space.ParseRef(p.Archive)
		if err != nil || ref.Type != lex.ArchiveType {
			return fmt.Errorf("archive must be a %s space URI, not %q", lex.ArchiveType, p.Archive)
		}
	}
	return nil
}

// CheckArchive reports what's missing before the principal can run an
// archive: one, and a sign-in that asked for it.
func (p Principal) CheckArchive() error {
	if p.Archive == "" {
		return errors.New("no archive yet: engram archive create <name>")
	}
	return p.checkArchiveScope()
}

func (p Principal) checkArchiveScope() error {
	for _, s := range p.Scopes {
		if strings.HasPrefix(s, "space:"+lex.ArchiveType) {
			return nil
		}
	}
	return errors.New("the principal's sign-in predates archives: run `engram principal login` again")
}

// CheckCreateArchive reports whether the principal's sign-in can create an
// archive.
func (p Principal) CheckCreateArchive() error { return p.checkArchiveScope() }

func (p Principal) stock() int {
	if p.Stock > 0 {
		return p.Stock
	}
	return 4
}

// Identity is one agent's own account: what an agent session signs in to
// the lounge with. It holds the account's password, so its file is readable
// only by its owner.
type Identity struct {
	DID       string `json:"did"`
	Handle    string `json:"handle"`
	Password  string `json:"password"`
	PDSHost   string `json:"pdsHost"`
	Principal string `json:"principal"`
	// Lounge is the lounge the agent was added to, and Archive the archive.
	Lounge  string `json:"lounge,omitempty"`
	Archive string `json:"archive,omitempty"`
	// Endorsed, Joined and Archived say whether the principal's
	// endorsement, the lounge membership and the archive membership are
	// done; a claim finishes what isn't.
	Endorsed  bool      `json:"endorsed"`
	Joined    bool      `json:"joined"`
	Archived  bool      `json:"archived,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	// Kind (session, subagent), Session and Parent are set when the
	// identity is claimed.
	Kind      string    `json:"kind,omitempty"`
	Session   string    `json:"session,omitempty"`
	Parent    string    `json:"parent,omitempty"`
	ClaimedAt time.Time `json:"claimedAt,omitzero"`
}

// Account is the identity as an agent account that signs in with its
// password.
func (id Identity) Account() Account {
	return Account{Handle: id.DID, DID: id.DID, SignIn: SignInPassword, Password: id.Password, PDSHost: id.PDSHost}
}

// LoadIdentity reads an identity file.
func LoadIdentity(path string) (Identity, error) {
	var id Identity
	raw, err := os.ReadFile(path)
	if err != nil {
		return id, err
	}
	if err := json.Unmarshal(raw, &id); err != nil {
		return id, fmt.Errorf("%s: %w", path, err)
	}
	if id.DID == "" || id.Password == "" || id.PDSHost == "" {
		return id, fmt.Errorf("%s: not an agent identity", path)
	}
	return id, nil
}

func writeIdentity(path string, id Identity) error {
	raw, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// IdentitiesDir is where agents' identity files live: agents/ in ConfigDir,
// with agents/stock/ holding the unclaimed ones.
func IdentitiesDir() (string, error) {
	d, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "agents"), nil
}

// Provisioner creates and hands out agent identities for a principal.
type Provisioner struct {
	Principal Principal
	// Session is the principal's own session at their PDS, which creates
	// agents and adds them to the lounge.
	Session *atclient.APIClient
	// Dir is the identities directory (IdentitiesDir).
	Dir string
	// Now is the clock (default time.Now).
	Now func() time.Time
}

func (pv *Provisioner) now() time.Time {
	if pv.Now != nil {
		return pv.Now()
	}
	return time.Now()
}

// Create makes a new agent account and finishes its setup. The principal's
// PDS creates the account, declared as the principal's agent, with the
// principal's recovery key, and endorses it, in one call; then the agent is
// added to the lounge. The identity file is written to the stock as soon as
// the account exists, so a later step's failure loses nothing; Claim
// finishes what's left.
func (pv *Provisioner) Create(ctx context.Context) (Identity, string, error) {
	p := pv.Principal
	if err := p.Check(); err != nil {
		return Identity{}, "", err
	}
	if pv.Session == nil || pv.Session.AccountDID == nil {
		return Identity{}, "", errors.New("no principal session")
	}
	body := map[string]any{}
	if p.RecoveryKey != "" {
		body["recoveryKey"] = p.RecoveryKey
	}
	var out struct {
		DID         string `json:"did"`
		Handle      string `json:"handle"`
		Password    string `json:"password"`
		Endorsement string `json:"endorsement"`
		Endorsed    bool   `json:"endorsed"`
	}
	if err := pv.Session.Post(ctx, CreateAgentNSID, body, &out); err != nil {
		var ae *atclient.APIError
		if errors.As(err, &ae) && (ae.StatusCode == http.StatusNotFound || ae.Name == "MethodNotImplemented") {
			return Identity{}, "", fmt.Errorf("the principal's PDS (%s) can't create agents (%s); it needs Cocoon with agent support", pv.Session.Host, CreateAgentNSID)
		}
		return Identity{}, "", fmt.Errorf("creating an agent account: %w", err)
	}
	if _, err := syntax.ParseDID(out.DID); err != nil || out.Password == "" {
		return Identity{}, "", fmt.Errorf("the PDS returned no usable agent (%q)", out.DID)
	}
	id := Identity{
		DID: out.DID, Handle: out.Handle, Password: out.Password, PDSHost: strings.TrimSuffix(pv.Session.Host, "/"),
		Principal: p.Account.DID, Lounge: p.Lounge, Archive: p.Archive, Endorsed: out.Endorsed, CreatedAt: pv.now().UTC(),
	}
	stock := filepath.Join(pv.Dir, "stock")
	if err := os.MkdirAll(stock, 0o700); err != nil {
		return id, "", err
	}
	path := filepath.Join(stock, id.DID+".json")
	if err := writeIdentity(path, id); err != nil {
		return id, "", err
	}
	id, err := pv.finish(ctx, path, id)
	return id, path, err
}

// finish endorses the agent and adds it to the lounge, whichever isn't done,
// recording progress in its file.
func (pv *Provisioner) finish(ctx context.Context, path string, id Identity) (Identity, error) {
	if !id.Endorsed {
		if err := Endorse(ctx, pv.Session, id.DID); err != nil {
			return id, err
		}
		id.Endorsed = true
		if err := writeIdentity(path, id); err != nil {
			return id, err
		}
	}
	if !id.Joined && id.Lounge != "" {
		body := map[string]any{"space": id.Lounge, "members": []Member{{DID: id.DID, Read: true, Write: true}}}
		if err := pv.Session.Post(ctx, "com.atproto.memberspace.putMembers", body, nil); err != nil {
			return id, manageErr("adding the agent to the lounge", err)
		}
		id.Joined = true
		if err := writeIdentity(path, id); err != nil {
			return id, err
		}
	}
	if !id.Archived && id.Archive != "" {
		body := map[string]any{"space": id.Archive, "members": []Member{{DID: id.DID, Read: true, Write: true}}}
		if err := pv.Session.Post(ctx, "com.atproto.memberspace.putMembers", body, nil); err != nil {
			return id, manageErr("adding the agent to the archive", err)
		}
		id.Archived = true
		if err := writeIdentity(path, id); err != nil {
			return id, err
		}
	}
	return id, nil
}

// Endorse writes the principal's link.ident.endorsement for agent: the half
// of the ident.link relationship the principal publishes.
func Endorse(ctx context.Context, principal *atclient.APIClient, agent string) error {
	if principal == nil || principal.AccountDID == nil {
		return errors.New("no principal session to endorse with")
	}
	body := map[string]any{
		"repo": principal.AccountDID.String(), "collection": lex.EndorsementCollection, "rkey": agent,
		"record": map[string]any{"$type": lex.EndorsementCollection, "agent": agent},
	}
	if err := principal.Post(ctx, "com.atproto.repo.putRecord", body, nil); err != nil {
		return fmt.Errorf("endorsing %s: %w", agent, err)
	}
	return nil
}

// Revoke deletes the principal's endorsement of agent, which ends the
// ident.link relationship.
func RevokeEndorsement(ctx context.Context, principal *atclient.APIClient, agent string) error {
	body := map[string]any{"repo": principal.AccountDID.String(), "collection": lex.EndorsementCollection, "rkey": agent}
	return principal.Post(ctx, "com.atproto.repo.deleteRecord", body, nil)
}

// ClaimIn says what an identity is claimed for.
type ClaimIn struct {
	Kind    string
	Session string
	Parent  string
}

// Claim hands out an identity: one from the stock when there is one (moved
// out of it, so no two claims get the same), else a new one. It finishes any
// setup the identity still needs, and returns it with its file's path.
// Every identity is used once: a claimed one never goes back to the stock.
func (pv *Provisioner) Claim(ctx context.Context, in ClaimIn) (Identity, string, error) {
	claimed := filepath.Join(pv.Dir, "claimed")
	if err := os.MkdirAll(claimed, 0o700); err != nil {
		return Identity{}, "", err
	}
	id, path, err := pv.takeStock(claimed)
	if err != nil {
		return Identity{}, "", err
	}
	if path == "" {
		var stockPath string
		if id, stockPath, err = pv.Create(ctx); err != nil {
			return id, "", err
		}
		path = filepath.Join(claimed, id.DID+".json")
		if err := os.Rename(stockPath, path); err != nil {
			return id, "", err
		}
	} else {
		// One stocked before the principal had this archive (or any) joins
		// the archive the principal uses now.
		if id.Archive != pv.Principal.Archive {
			id.Archive, id.Archived = pv.Principal.Archive, false
		}
		if id, err = pv.finish(ctx, path, id); err != nil {
			return id, path, err
		}
	}
	id.Kind, id.Session, id.Parent, id.ClaimedAt = in.Kind, in.Session, in.Parent, pv.now().UTC()
	return id, path, writeIdentity(path, id)
}

// takeStock moves the oldest stocked identity into dst. The rename is the
// claim: of two processes, only one moves each file.
func (pv *Provisioner) takeStock(dst string) (Identity, string, error) {
	stock := filepath.Join(pv.Dir, "stock")
	entries, err := os.ReadDir(stock)
	if errors.Is(err, os.ErrNotExist) {
		return Identity{}, "", nil
	}
	if err != nil {
		return Identity{}, "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		from, to := filepath.Join(stock, e.Name()), filepath.Join(dst, e.Name())
		if err := os.Rename(from, to); err != nil {
			continue // another process took it
		}
		id, err := LoadIdentity(to)
		if err != nil {
			continue
		}
		return id, to, nil
	}
	return Identity{}, "", nil
}

// StockCount is how many unclaimed identities are ready.
func (pv *Provisioner) StockCount() int {
	entries, _ := os.ReadDir(filepath.Join(pv.Dir, "stock"))
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

// Fill creates identities until the stock holds want (default: the
// principal's Stock), and returns how many it made.
func (pv *Provisioner) Fill(ctx context.Context, want int) (int, error) {
	if want <= 0 {
		want = pv.Principal.stock()
	}
	made := 0
	for pv.StockCount() < want {
		if _, _, err := pv.Create(ctx); err != nil {
			return made, err
		}
		made++
	}
	return made, nil
}

// ---- verification ----

// Principals returns the principals the agent declares in its DID document
// (ident.link agent-of entries), each with whether its endorsement
// confirms it. A relationship is confirmed only when both halves exist. A
// check that couldn't be made (a DID or PDS that didn't answer) is an error,
// not an unconfirmed relationship.
func Principals(ctx context.Context, dir identity.Directory, httpc *http.Client, agent string) (map[string]bool, error) {
	d, err := syntax.ParseDID(agent)
	if err != nil {
		return nil, err
	}
	ident, err := dir.LookupDID(ctx, d)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, aka := range ident.AlsoKnownAs {
		rest, ok := strings.CutPrefix(aka, lex.IdentLinkAgentOf)
		if !ok {
			continue
		}
		pd, err := syntax.ParseDID(rest)
		if err != nil {
			continue
		}
		confirmed, err := endorses(ctx, dir, httpc, pd, agent)
		if err != nil {
			return nil, fmt.Errorf("checking %s's endorsement: %w", pd, err)
		}
		out[pd.String()] = confirmed
	}
	return out, nil
}

// endorses reports whether principal's repo holds an endorsement of agent.
// A principal whose DID doesn't resolve, or a missing record, is an answer
// (no); a server that fails is an error.
func endorses(ctx context.Context, dir identity.Directory, httpc *http.Client, principal syntax.DID, agent string) (bool, error) {
	pi, err := dir.LookupDID(ctx, principal)
	if errors.Is(err, identity.ErrDIDNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if pi.PDSEndpoint() == "" {
		return false, nil
	}
	api := atclient.NewAPIClient(pi.PDSEndpoint())
	if httpc != nil {
		api.Client = httpc
	}
	var out struct {
		Value map[string]any `json:"value"`
	}
	params := map[string]any{"repo": principal.String(), "collection": lex.EndorsementCollection, "rkey": agent}
	if err := api.Get(ctx, "com.atproto.repo.getRecord", params, &out); err != nil {
		var ae *atclient.APIError
		if errors.As(err, &ae) && (ae.Name == "RecordNotFound" || ae.StatusCode == http.StatusNotFound) {
			return false, nil
		}
		return false, err
	}
	got, _ := out.Value["agent"].(string)
	return got == agent, nil
}

// ---- lounges the principal runs ----

// CreateLounge creates a lounge governed by the principal (a member list:
// only members read and write) and returns its URI.
func CreateLounge(ctx context.Context, principal *atclient.APIClient, name string) (string, error) {
	return createPrincipalSpace(ctx, principal, lex.LoungeType, name, "creating the lounge")
}

// CreateArchive creates an archive governed by the principal, where agents
// keep their sessions, and returns its URI.
func CreateArchive(ctx context.Context, principal *atclient.APIClient, name string) (string, error) {
	return createPrincipalSpace(ctx, principal, lex.ArchiveType, name, "creating the archive")
}

func createPrincipalSpace(ctx context.Context, principal *atclient.APIClient, spaceType, name, doing string) (string, error) {
	if _, err := syntax.ParseRecordKey(name); err != nil || name == "." || name == ".." {
		return "", errors.New("names use letters, digits and . _ ~ : - (up to 512)")
	}
	body := map[string]any{"spaceType": spaceType, "skey": name, "policy": map[string]any{"$type": "com.atproto.memberspace.policy"}}
	var out struct {
		Space string `json:"space"`
	}
	if err := principal.Post(ctx, "com.atproto.space.createSpace", body, &out); err != nil {
		return "", manageErr(doing, err)
	}
	return out.Space, nil
}

// AddToLounge adds DIDs to a lounge the principal governs, as members who
// read and write.
func AddToLounge(ctx context.Context, principal *atclient.APIClient, lounge string, dids ...string) error {
	members := make([]Member, len(dids))
	for i, d := range dids {
		members[i] = Member{DID: d, Read: true, Write: true}
	}
	body := map[string]any{"space": lounge, "members": members}
	if err := principal.Post(ctx, "com.atproto.memberspace.putMembers", body, nil); err != nil {
		return manageErr("adding to the lounge", err)
	}
	return nil
}

// GrantLink is the appview's page where the space's authority lets it read
// the space (mode grant) or stops it (mode stop).
func GrantLink(ctx context.Context, appviewURL string, httpc *http.Client, spaceURI, mode string) (string, error) {
	c := atclient.NewAPIClient(appviewURL)
	if httpc != nil {
		c.Client = httpc
	}
	var out struct {
		GrantURL string `json:"grantUrl"`
	}
	if err := c.Get(ctx, "garden.engram.describeService", nil, &out); err != nil {
		return "", fmt.Errorf("asking the appview at %s: %w", appviewURL, err)
	}
	if out.GrantURL == "" {
		return "", fmt.Errorf("the appview at %s doesn't take indexing grants", appviewURL)
	}
	return out.GrantURL + "?space=" + url.QueryEscape(spaceURI) + "&mode=" + mode, nil
}

// DeclareArchiveModel writes the archive's garden.engram.config: the model
// the appview embeds its passages with. Changing it later re-embeds every
// passage.
func DeclareArchiveModel(ctx context.Context, principal *atclient.APIClient, archive string, m lex.ModelInfo) (lex.Config, error) {
	ref, err := space.ParseRef(archive)
	if err != nil {
		return lex.Config{}, err
	}
	if ref.Type != lex.ArchiveType {
		return lex.Config{}, fmt.Errorf("%s isn't an archive", archive)
	}
	cfg, err := lex.ChangeConfig(nil, lex.Declare, m, "", "")
	if err != nil {
		return cfg, err
	}
	body := map[string]any{
		"space": archive, "repo": ref.Authority, "collection": lex.ConfigCollection, "rkey": lex.ConfigRkey,
		"record": cfg.Record(time.Now()),
	}
	if err := principal.Post(ctx, "com.atproto.space.putRecord", body, nil); err != nil {
		return cfg, manageErr("declaring the archive's model", err)
	}
	return cfg, nil
}
