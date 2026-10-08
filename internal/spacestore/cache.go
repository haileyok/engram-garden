package spacestore

import (
	"container/list"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/haileyok/engram-garden/internal/blob"
)

// diskCache keeps whole segment files on local disk, evicting the least
// recently used segment when over budget.
type diskCache struct {
	dir    string
	budget int64

	mu    sync.Mutex
	used  int64
	lru   *list.List // of *cachedFile, most recent at front
	files map[string]*list.Element
}

type cachedFile struct {
	key  string // object key
	path string
	size int64
	f    *os.File
	refs int // open readers; evicted files are closed when refs drop to 0
	gone bool
}

func newDiskCache(dir string, budget int64) *diskCache {
	return &diskCache{dir: dir, budget: budget, lru: list.New(), files: map[string]*list.Element{}}
}

func (c *diskCache) pathFor(key string) string {
	return filepath.Join(c.dir, filepath.FromSlash(key))
}

// acquire returns the open cached file for key, if present.
func (c *diskCache) acquire(key string) *cachedFile {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.files[key]
	if !ok {
		return nil
	}
	c.lru.MoveToFront(el)
	cf := el.Value.(*cachedFile)
	cf.refs++
	return cf
}

func (c *diskCache) release(cf *cachedFile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cf.refs--
	if cf.gone && cf.refs == 0 {
		cf.f.Close()
	}
}

// add installs a finished local file (moved into the cache) for key.
func (c *diskCache) add(key, src string) error {
	if c == nil {
		return os.Remove(src)
	}
	dst := c.pathFor(key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	f, err := os.Open(dst)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.files[key]; ok {
		c.drop(el)
	}
	cf := &cachedFile{key: key, path: dst, size: st.Size(), f: f}
	c.files[key] = c.lru.PushFront(cf)
	c.used += cf.size
	for c.used > c.budget && c.lru.Len() > 1 {
		c.drop(c.lru.Back())
	}
	return nil
}

func (c *diskCache) drop(el *list.Element) {
	cf := el.Value.(*cachedFile)
	c.lru.Remove(el)
	delete(c.files, cf.key)
	c.used -= cf.size
	cf.gone = true
	_ = os.Remove(cf.path) // open handles keep working until closed
	if cf.refs == 0 {
		cf.f.Close()
	}
}

// remove drops a key, as when its segment is merged away.
func (c *diskCache) remove(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.files[key]; ok {
		c.drop(el)
	}
}

func (c *diskCache) has(key string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.files[key]
	return ok
}

// fetch downloads a whole object into the cache.
func (c *diskCache) fetch(ctx context.Context, bs blob.Store, key string) error {
	if c == nil || c.has(key) {
		return nil
	}
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return err
	}
	rc, err := bs.Get(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()
	f, err := os.CreateTemp(c.dir, ".dl-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := c.add(key, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// source reads one segment: from the disk cache when it holds the file,
// otherwise with range reads from object storage.
type source struct {
	key   string
	size  int64
	blob  blob.Store
	cache *diskCache
	// remote counts bytes read from object storage, for tests and metrics.
	remote *counter
}

type counter struct {
	mu          sync.Mutex
	reads, byts int64
}

func (c *counter) add(n int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.reads++
	c.byts += n
	c.mu.Unlock()
}

func (s *source) Size() int64 { return s.size }

func (s *source) ReadRange(ctx context.Context, off, n int64) ([]byte, error) {
	if cf := s.cache.acquire(s.key); cf != nil {
		defer s.cache.release(cf)
		b := make([]byte, n)
		_, err := cf.f.ReadAt(b, off)
		if err == nil || (errors.Is(err, io.EOF) && n == 0) {
			segmentReads.WithLabelValues("disk_cache").Inc()
			segmentReadBytes.WithLabelValues("disk_cache").Add(float64(n))
			return b, nil
		}
		// Fall through to object storage on a local read error.
	}
	s.remote.add(n)
	segmentReads.WithLabelValues("object_storage").Inc()
	segmentReadBytes.WithLabelValues("object_storage").Add(float64(n))
	return s.blob.GetRange(ctx, s.key, off, n)
}
