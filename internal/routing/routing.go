// Package routing decides which appview node owns each space: rendezvous
// hashing of the space over the node list, with the list's epoch as the
// fencing token.
package routing

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/haileyok/engram-garden/internal/blob"
	"github.com/haileyok/engram-garden/internal/spacestore"
)

// Node is one appview node.
type Node struct {
	ID string `json:"id"`
	// URL is where other nodes reach it (internal HTTP).
	URL string `json:"url"`
}

// Ring is the node list as this node sees it.
type Ring struct {
	Self  string
	Nodes []Node
	// Epoch increases on every change to the node list. It's the fencing
	// token until a coordination store hands out leases.
	Epoch uint64
}

// Single is a ring of one node, for single-node deployments.
func Single() *Ring { return &Ring{Self: "self", Nodes: []Node{{ID: "self"}}, Epoch: 1} }

// ParseNodes reads "id=url,id=url".
func ParseNodes(s string) ([]Node, error) {
	var out []Node
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, u, ok := strings.Cut(part, "=")
		if !ok || id == "" {
			return nil, fmt.Errorf("node %q: want id=url", part)
		}
		pu, err := url.Parse(u)
		if err != nil || pu.Host == "" {
			return nil, fmt.Errorf("node %s: bad url %q", id, u)
		}
		if seen[id] {
			return nil, fmt.Errorf("node %s listed twice", id)
		}
		seen[id] = true
		out = append(out, Node{ID: id, URL: strings.TrimSuffix(u, "/")})
	}
	if len(out) == 0 {
		return nil, errors.New("no nodes")
	}
	return out, nil
}

func score(key, nodeID string) uint64 {
	h := sha256.Sum256([]byte(key + "\x00" + nodeID))
	return binary.BigEndian.Uint64(h[:8])
}

// OwnerOf returns the node that owns a key (a space URI, or "registry"):
// the node with the highest hash of key and node id. Every node computes
// the same answer, and adding or removing a node only moves the keys whose
// owner changes.
func (r *Ring) OwnerOf(key string) Node {
	best, bestScore := Node{}, uint64(0)
	for i, n := range r.Nodes {
		if s := score(key, n.ID); i == 0 || s > bestScore || (s == bestScore && n.ID < best.ID) {
			best, bestScore = n, s
		}
	}
	return best
}

// Owner returns the node that owns a space.
func (r *Ring) Owner(spaceURI string) Node { return r.OwnerOf(spacestore.SpaceKey(spaceURI)) }

// Owns reports whether this node owns a space.
func (r *Ring) Owns(spaceURI string) bool { return r.Owner(spaceURI).ID == r.Self }

// Lease is spacestore.Options.Lease: the epoch, if this node owns the space.
func (r *Ring) Lease(spaceURI string) (uint64, bool) { return r.Epoch, r.Owns(spaceURI) }

// Registry is the small global list of indexed spaces and their owners. It
// is rebuildable: the spaces come from configuration, and owners renew
// their notification registrations regardless.
type Registry struct {
	Format     int       `json:"format"`
	Epoch      uint64    `json:"epoch"`
	Generation uint64    `json:"generation"`
	Nodes      []Node    `json:"nodes"`
	Spaces     []Entry   `json:"spaces"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Entry is one indexed space.
type Entry struct {
	Space string `json:"space"`
	Key   string `json:"key"`
	Owner string `json:"owner"`
}

// IsCoordinator reports whether this node maintains the registry.
func (r *Ring) IsCoordinator() bool { return r.OwnerOf("registry").ID == r.Self }

func registryKey(epoch, gen uint64) string {
	return fmt.Sprintf("registry-%020d-%020d.json", epoch, gen)
}

// LoadRegistry reads the newest registry, ordered like manifests: highest
// epoch, then generation. It returns nil when there is none.
func LoadRegistry(ctx context.Context, bs blob.Store) (*Registry, error) {
	objs, err := bs.List(ctx, "registry-")
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(objs))
	for _, o := range objs {
		keys = append(keys, o.Key)
	}
	if len(keys) == 0 {
		return nil, nil
	}
	sort.Strings(keys) // zero-padded, so lexical order is numeric order
	raw, err := blob.GetBytes(ctx, bs, keys[len(keys)-1])
	if err != nil {
		return nil, err
	}
	var reg Registry
	if err := json.Unmarshal(raw, &reg); err != nil {
		return nil, err
	}
	return &reg, nil
}

// WriteRegistry publishes the registry when this node is the coordinator
// and the contents changed. Older registries are left for garbage
// collection with the provider's minimum retention in mind.
func (r *Ring) WriteRegistry(ctx context.Context, bs blob.Store, spaces []string) (*Registry, error) {
	if !r.IsCoordinator() {
		return nil, nil
	}
	cur, err := LoadRegistry(ctx, bs)
	if err != nil {
		return nil, err
	}
	next := &Registry{Format: 1, Epoch: r.Epoch, Nodes: r.Nodes, UpdatedAt: time.Now().UTC()}
	for _, sp := range spaces {
		next.Spaces = append(next.Spaces, Entry{Space: sp, Key: spacestore.SpaceKey(sp), Owner: r.Owner(sp).ID})
	}
	sort.Slice(next.Spaces, func(i, j int) bool { return next.Spaces[i].Space < next.Spaces[j].Space })
	if cur != nil {
		if cur.Epoch > r.Epoch {
			return nil, fmt.Errorf("registry epoch %d is newer than ours (%d)", cur.Epoch, r.Epoch)
		}
		if cur.Epoch == r.Epoch {
			next.Generation = cur.Generation
			a, _ := json.Marshal(struct {
				N []Node
				S []Entry
			}{cur.Nodes, cur.Spaces})
			b, _ := json.Marshal(struct {
				N []Node
				S []Entry
			}{next.Nodes, next.Spaces})
			if string(a) == string(b) {
				return cur, nil
			}
		}
	}
	next.Generation++
	raw, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	if err := blob.PutBytes(ctx, bs, registryKey(next.Epoch, next.Generation), raw, false); err != nil {
		return nil, err
	}
	return next, nil
}
