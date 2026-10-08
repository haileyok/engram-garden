// Package routing decides which appview node owns each space: rendezvous
// hashing of the space over the node list, with the list's epoch as the
// fencing token.
package routing

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"

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

// OwnerOf returns the node that owns a key (a space URI, or "coordinator"):
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

// IsCoordinator reports whether this node does the jobs that one node
// should do for all of them, such as clearing abandoned sign-ins.
func (r *Ring) IsCoordinator() bool { return r.OwnerOf("coordinator").ID == r.Self }
