package spacestore

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/haileyok/engram-garden/internal/segment"
)

// ExportManifestName is the manifest's name inside an export.
const ExportManifestName = "manifest.json"

// Export writes a tar of the space's newest manifest (manifest.json) and
// its segments (seg-{id}.seg), after flushing the buffer. The export format
// is the storage format.
func (n *Node) Export(ctx context.Context, spaceURI string, w io.Writer) error {
	s, err := n.space(ctx, spaceURI)
	if err != nil {
		return err
	}
	// Hold the flush lock so no merge deletes a segment mid-export.
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	// Only the current owner's state is authoritative: a node that lost the
	// space mustn't hand out its stale copy.
	if err := s.checkLease(); err != nil {
		return err
	}
	if err := s.flushLocked(ctx); err != nil {
		return err
	}
	s.mu.RLock()
	man := s.man.clone()
	s.mu.RUnlock()
	tw := tar.NewWriter(w)
	mb := manifestBytes(man)
	now := time.Now()
	if err := tw.WriteHeader(&tar.Header{Name: ExportManifestName, Mode: 0o644, Size: int64(len(mb)), ModTime: now}); err != nil {
		return err
	}
	if _, err := tw.Write(mb); err != nil {
		return err
	}
	for _, im := range []*IndexManifest{man.Active, man.Building} {
		if im == nil {
			continue
		}
		for _, si := range im.Segments {
			rc, err := n.opt.Blob.Get(ctx, SegmentKey(spaceURI, si.ID))
			if err != nil {
				return fmt.Errorf("segment %s: %w", si.ID, err)
			}
			err = tw.WriteHeader(&tar.Header{Name: "seg-" + si.ID + ".seg", Mode: 0o644, Size: si.Bytes, ModTime: now})
			if err == nil {
				_, err = io.CopyN(tw, rc, si.Bytes)
			}
			rc.Close()
			if err != nil {
				return fmt.Errorf("segment %s: %w", si.ID, err)
			}
		}
	}
	return tw.Close()
}

// Import reads an export, checks the manifest and every segment, uploads
// them under this node's storage and publishes the manifest under this
// node's token. Indexing then resumes from the manifest's repo positions.
// It returns the imported space's URI.
func (n *Node) Import(ctx context.Context, r io.Reader) (string, error) {
	tr := tar.NewReader(r)
	var man *Manifest
	seen := map[string]bool{}
	tmp, err := os.MkdirTemp(n.opt.CacheDir, ".import-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	type staged struct{ id, path string }
	var segs []staged
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("reading export: %w", err)
		}
		name := path.Base(h.Name)
		switch {
		case name == ExportManifestName:
			raw, err := io.ReadAll(io.LimitReader(tr, 64<<20))
			if err != nil {
				return "", err
			}
			if man, err = DecodeManifest(raw); err != nil {
				return "", fmt.Errorf("manifest: %w", err)
			}
		case strings.HasPrefix(name, "seg-") && strings.HasSuffix(name, ".seg"):
			id := strings.TrimSuffix(strings.TrimPrefix(name, "seg-"), ".seg")
			if id == "" || strings.ContainsAny(id, "/.") || seen[id] {
				return "", fmt.Errorf("bad or duplicate segment %q", name)
			}
			seen[id] = true
			p := path.Join(tmp, id)
			f, err := os.Create(p)
			if err != nil {
				return "", err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return "", err
			}
			segs = append(segs, staged{id, p})
		default:
			return "", fmt.Errorf("unexpected entry %q in export", h.Name)
		}
	}
	if man == nil {
		return "", errors.New("export has no manifest.json")
	}
	token, ok := n.opt.Lease(man.Space)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrNotOwner, man.Space)
	}
	want := map[string]SegmentInfo{}
	for _, im := range []*IndexManifest{man.Active, man.Building} {
		if im == nil {
			continue
		}
		for _, si := range im.Segments {
			want[si.ID] = si
			if !seen[si.ID] {
				return "", fmt.Errorf("export is missing segment %s", si.ID)
			}
		}
	}
	// Check every segment before uploading anything.
	for _, sg := range segs {
		si, ok := want[sg.id]
		if !ok {
			return "", fmt.Errorf("segment %s isn't in the manifest", sg.id)
		}
		fr, err := segment.OpenFile(sg.path)
		if err != nil {
			return "", err
		}
		rd, err := segment.Open(ctx, fr)
		if err == nil {
			err = rd.Verify(ctx)
		}
		if err == nil && (rd.Count() != si.Count || fr.Size() != si.Bytes) {
			err = fmt.Errorf("doesn't match the manifest (%d memories, %d bytes)", rd.Count(), fr.Size())
		}
		fr.Close()
		if err != nil {
			return "", fmt.Errorf("segment %s: %w", sg.id, err)
		}
	}
	cur, err := LoadLatestManifest(ctx, n.opt.Blob, man.Space)
	switch {
	case errors.Is(err, ErrNoManifest):
		man.Generation = 1
	case err != nil:
		return "", err
	case cur.Token > token:
		return "", fmt.Errorf("%w (stored token %d, ours %d)", ErrStaleOwner, cur.Token, token)
	case cur.Token == token:
		man.Generation = cur.Generation + 1
	default:
		man.Generation = 1
	}
	for _, sg := range segs {
		f, err := os.Open(sg.path)
		if err != nil {
			return "", err
		}
		err = n.opt.Blob.Put(ctx, SegmentKey(man.Space, sg.id), f, want[sg.id].Bytes)
		f.Close()
		if err != nil {
			return "", fmt.Errorf("uploading segment %s: %w", sg.id, err)
		}
	}
	man.Token = token
	man.UpdatedAt = n.now().UTC()
	if err := n.putManifest(ctx, man); err != nil {
		return "", err
	}
	// Drop any loaded copy so the next request reads the import.
	n.mu.Lock()
	delete(n.spaces, man.Space)
	n.mu.Unlock()
	return man.Space, nil
}
