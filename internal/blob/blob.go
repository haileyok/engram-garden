// Package blob is the object storage the index lives in: a local directory
// for development and single-node use, or an S3-compatible bucket (Wasabi in
// production).
package blob

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var (
	// ErrNotFound reports a missing object.
	ErrNotFound = errors.New("object not found")
	// ErrExists reports a conditional write that found the key taken.
	ErrExists = errors.New("object already exists")
)

// Object describes a stored object.
type Object struct {
	Key      string
	Size     int64
	Modified time.Time
}

// Store is object storage. Objects are written whole and never modified.
type Store interface {
	// Put writes an object, replacing any existing one.
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	// PutIfAbsent writes an object only if the key is free, returning
	// ErrExists otherwise. Backends that can't guarantee this must not be
	// trusted with it: see Probe.
	PutIfAbsent(ctx context.Context, key string, r io.Reader, size int64) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// GetRange reads n bytes at off.
	GetRange(ctx context.Context, key string, off, n int64) ([]byte, error)
	// Size returns an object's size.
	Size(ctx context.Context, key string) (int64, error)
	// List returns the objects under a prefix, sorted by key.
	List(ctx context.Context, prefix string) ([]Object, error)
	Delete(ctx context.Context, key string) error
}

// PutBytes writes a small object.
func PutBytes(ctx context.Context, s Store, key string, b []byte, ifAbsent bool) error {
	if ifAbsent {
		return s.PutIfAbsent(ctx, key, bytes.NewReader(b), int64(len(b)))
	}
	return s.Put(ctx, key, bytes.NewReader(b), int64(len(b)))
}

// GetBytes reads a whole object.
func GetBytes(ctx context.Context, s Store, key string) ([]byte, error) {
	rc, err := s.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Probe checks whether the store really honors conditional writes: it
// writes a probe key, then writes it again conditionally. Some S3-compatible
// providers ignore the condition and overwrite, so only a rejected second
// write counts.
func Probe(ctx context.Context, s Store) (bool, error) {
	key := fmt.Sprintf("probe/conditional-write-%d-%s", time.Now().UnixNano(), randomHex(8))
	if err := PutBytes(ctx, s, key, []byte("first"), false); err != nil {
		return false, fmt.Errorf("probe write: %w", err)
	}
	defer s.Delete(context.WithoutCancel(ctx), key) //nolint:errcheck
	err := PutBytes(ctx, s, key, []byte("second"), true)
	switch {
	case errors.Is(err, ErrExists):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("probe conditional write: %w", err)
	}
	return false, nil
}

// Dir stores objects as files under a directory.
type Dir struct{ Root string }

func (d Dir) path(key string) (string, error) {
	if key == "" || strings.Contains(key, "..") || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") {
		return "", fmt.Errorf("bad object key %q", key)
	}
	return filepath.Join(d.Root, filepath.FromSlash(key)), nil
}

// write stages the object in a temp file, then moves (or links, for
// conditional writes) it into place, so readers never see a partial object.
func (d Dir) write(key string, r io.Reader, ifAbsent bool) error {
	p, err := d.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if ifAbsent {
		// link fails if the target exists, atomically.
		if err := os.Link(tmp, p); err != nil {
			if errors.Is(err, os.ErrExist) {
				return ErrExists
			}
			return err
		}
		return nil
	}
	return os.Rename(tmp, p)
}

func (d Dir) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	return d.write(key, r, false)
}

func (d Dir) PutIfAbsent(_ context.Context, key string, r io.Reader, _ int64) error {
	return d.write(key, r, true)
}

func (d Dir) Get(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := d.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

func (d Dir) GetRange(_ context.Context, key string, off, n int64) ([]byte, error) {
	p, err := d.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, n)
	if _, err := f.ReadAt(b, off); err != nil && !(errors.Is(err, io.EOF) && n == 0) {
		return nil, err
	}
	return b, nil
}

func (d Dir) Size(_ context.Context, key string) (int64, error) {
	p, err := d.path(key)
	if err != nil {
		return 0, err
	}
	st, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

func (d Dir) List(_ context.Context, prefix string) ([]Object, error) {
	var out []Object
	err := filepath.WalkDir(d.Root, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if e.IsDir() || strings.HasPrefix(e.Name(), ".tmp-") {
			return nil
		}
		rel, _ := filepath.Rel(d.Root, p)
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return nil // removed meanwhile
		}
		out = append(out, Object{Key: key, Size: info.Size(), Modified: info.ModTime()})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, err
}

func (d Dir) Delete(_ context.Context, key string) error {
	p, err := d.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
