package archive

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
)

// dirCacheSize bounds the number of directory handles kept open.
const dirCacheSize = 16

// dirCache keeps recently used directories of an extraction open as
// os.Roots. Every os.Root operation walks its path one component at a
// time, so working through a cached parent turns several openat calls per
// file into one. Sub-roots are opened through the main root and are
// confined to it in the same way.
//
// Only the extracting goroutine calls dirCache methods; write-pool
// goroutines hold references to the roots they use, so an evicted root
// stays open until they are done with it.
type dirCache struct {
	root    *sharedRoot
	entries []dirCacheEntry // most recently used first
}

type dirCacheEntry struct {
	name string
	root *sharedRoot
}

// sharedRoot is a reference-counted os.Root, closed when the last
// reference is released.
type sharedRoot struct {
	*os.Root
	mu   sync.Mutex
	refs int
}

func newSharedRoot(r *os.Root) *sharedRoot { return &sharedRoot{Root: r, refs: 1} }

func (r *sharedRoot) acquire() {
	r.mu.Lock()
	r.refs++
	r.mu.Unlock()
}

func (r *sharedRoot) release() {
	r.mu.Lock()
	r.refs--
	last := r.refs == 0
	r.mu.Unlock()
	if last {
		r.Root.Close()
	}
}

// newDirCache takes ownership of root.
func newDirCache(root *os.Root) *dirCache {
	return &dirCache{root: newSharedRoot(root)}
}

// get returns a root for the directory dir, creating it if needed.
func (c *dirCache) get(dir string) (*sharedRoot, error) {
	if dir == "." {
		return c.root, nil
	}
	for i, e := range c.entries {
		if e.name == dir {
			copy(c.entries[1:i+1], c.entries[:i])
			c.entries[0] = e
			return e.root, nil
		}
	}
	sub, err := c.open(dir, 0o755)
	if err != nil {
		return nil, err
	}
	if len(c.entries) == dirCacheSize {
		c.entries[len(c.entries)-1].root.release()
		c.entries = c.entries[:len(c.entries)-1]
	}
	e := dirCacheEntry{dir, newSharedRoot(sub)}
	c.entries = slices.Insert(c.entries, 0, e)
	return e.root, nil
}

// mkdir creates the directory dir (and its parents) with mode perm if it
// does not exist yet.
func (c *dirCache) mkdir(dir string, perm fs.FileMode) error {
	parent, err := c.get(path.Dir(dir))
	if err == nil {
		err = parent.Mkdir(path.Base(dir), perm)
		if errors.Is(err, fs.ErrExist) {
			err = nil
		}
	}
	if err != nil {
		// Through the main root, which also resolves symlinks to
		// directories elsewhere in the tree.
		return c.root.MkdirAll(dir, perm)
	}
	return nil
}

// open creates dir if needed and opens it as a root. It works relative to
// the cached parent, so each step is a single-component lookup.
func (c *dirCache) open(dir string, perm fs.FileMode) (*os.Root, error) {
	if parent, err := c.get(path.Dir(dir)); err == nil {
		base := path.Base(dir)
		err := parent.Mkdir(base, perm)
		if err == nil || errors.Is(err, fs.ErrExist) {
			if sub, err := parent.OpenRoot(base); err == nil {
				return sub, nil
			}
		}
	}
	// A sub-root cannot follow a symlink out of itself, even to elsewhere
	// in the tree; the main root can.
	if err := c.root.MkdirAll(dir, perm); err != nil {
		return nil, err
	}
	return c.root.OpenRoot(dir)
}

// prepare makes way for a new non-directory entry at name: it creates the
// parent directories and removes any existing non-directory there, so a
// later entry replaces an earlier one as it does with tar. It returns the
// parent's root and the entry's base name.
func (c *dirCache) prepare(name string) (*sharedRoot, string, error) {
	sub, err := c.get(path.Dir(name))
	if err != nil {
		return nil, "", err
	}
	base := path.Base(name)
	fi, err := sub.Lstat(base)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return sub, base, nil
	case err != nil:
		return nil, "", err
	case fi.IsDir():
		return nil, "", fmt.Errorf("%s: archive replaces a directory with a file", name)
	}
	// A replaced symlink may have been the path to cached directories.
	c.forget(name)
	return sub, base, sub.Remove(base)
}

// forget drops cached handles for name and anything below it.
func (c *dirCache) forget(name string) {
	c.entries = slices.DeleteFunc(c.entries, func(e dirCacheEntry) bool {
		if e.name == name || strings.HasPrefix(e.name, name+"/") {
			e.root.release()
			return true
		}
		return false
	})
}

// close releases the cache's references, including the main root.
func (c *dirCache) close() {
	for _, e := range c.entries {
		e.root.release()
	}
	c.entries = nil
	c.root.release()
}
