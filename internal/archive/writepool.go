package archive

import (
	"bytes"
	"context"
	"io/fs"
	"sync"
	"time"
)

// writePool writes small extracted files on several goroutines. Creating
// many small files is dominated by per-file system calls, which overlap
// well: extracting a large source tree was about 2.5x faster with 4
// writers than with 1.
type writePool struct {
	ctx  context.Context
	jobs chan writeJob
	wg   sync.WaitGroup // running workers

	mu       sync.Mutex
	inflight map[string]struct{} // archive names queued or being written
	idle     *sync.Cond          // signalled when inflight empties
	err      error
}

type writeJob struct {
	name  string
	dir   *sharedRoot
	base  string
	data  []byte
	perm  fs.FileMode
	mtime time.Time
}

func newWritePool(ctx context.Context, workers int) *writePool {
	p := &writePool{
		ctx:      ctx,
		jobs:     make(chan writeJob, 4*workers),
		inflight: map[string]struct{}{},
	}
	p.idle = sync.NewCond(&p.mu)
	for range workers {
		p.wg.Go(p.work)
	}
	return p
}

func (p *writePool) work() {
	for j := range p.jobs {
		err := p.ctx.Err()
		if err == nil && p.failed() == nil {
			_, err = writeFile(j.dir.Root, j.base, bytes.NewReader(j.data), j.perm, j.mtime)
		}
		j.dir.release()

		p.mu.Lock()
		if err != nil && p.err == nil {
			p.err = err
		}
		delete(p.inflight, j.name)
		if len(p.inflight) == 0 {
			p.idle.Broadcast()
		}
		p.mu.Unlock()
	}
}

// add queues a file to be written; j.name is its path in the archive. The
// pool holds a reference to j.dir until the write is done.
func (p *writePool) add(j writeJob) {
	j.dir.acquire()
	p.mu.Lock()
	p.inflight[j.name] = struct{}{}
	p.mu.Unlock()
	p.jobs <- j
}

// pending reports whether a write to the archive path name is in flight.
func (p *writePool) pending(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.inflight[name]
	return ok
}

func (p *writePool) failed() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// wait blocks until all queued writes are done and returns the first
// error.
func (p *writePool) wait() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.inflight) > 0 {
		p.idle.Wait()
	}
	return p.err
}

// close finishes the queued writes, stops the workers and returns the
// first error.
func (p *writePool) close() error {
	close(p.jobs)
	p.wg.Wait()
	return p.failed()
}
