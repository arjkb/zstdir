package archive

import (
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// chunkSize is the amount of input compressed into each independent zstd
// frame by parallelEncoder.
const chunkSize = 4 << 20

// parallelEncoder compresses a stream on several goroutines. The zstd
// stream encoder keeps only about one core busy, so instead the input is
// cut into chunkSize pieces that are compressed concurrently as
// independent zstd frames and written out in order. Concatenated frames are
// a valid zstd stream (this is what pzstd does); the cost is a slightly
// lower ratio, since matches cannot span chunks.
type parallelEncoder struct {
	enc   *zstd.Encoder
	cur   *chunk // chunk being filled
	jobs  chan *chunk
	order chan *chunk
	pool  sync.Pool
	wg    sync.WaitGroup
	done  chan struct{}

	mu  sync.Mutex
	err error
}

type chunk struct {
	in, out []byte
	ready   chan struct{}
}

func newParallelEncoder(w io.Writer, level zstd.EncoderLevel, threads int) (*parallelEncoder, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(level), zstd.WithEncoderConcurrency(threads))
	if err != nil {
		return nil, err
	}
	p := &parallelEncoder{
		enc: enc,
		// Bound the chunks in flight, and with them memory use.
		order: make(chan *chunk, threads+1),
		jobs:  make(chan *chunk),
		done:  make(chan struct{}),
	}
	for range threads {
		p.wg.Go(func() {
			for c := range p.jobs {
				c.out = enc.EncodeAll(c.in, c.out[:0])
				close(c.ready)
			}
		})
	}
	go func() {
		defer close(p.done)
		for c := range p.order {
			<-c.ready
			if p.failed() == nil {
				if _, err := w.Write(c.out); err != nil {
					p.fail(err)
				}
			}
			p.pool.Put(c)
		}
	}()
	return p, nil
}

func (p *parallelEncoder) fail(err error) {
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.mu.Unlock()
}

func (p *parallelEncoder) failed() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *parallelEncoder) Write(b []byte) (int, error) {
	if err := p.failed(); err != nil {
		return 0, err
	}
	n := len(b)
	for len(b) > 0 {
		if p.cur == nil {
			p.cur = p.newChunk()
		}
		k := min(len(b), chunkSize-len(p.cur.in))
		p.cur.in = append(p.cur.in, b[:k]...)
		b = b[k:]
		if len(p.cur.in) == chunkSize {
			p.submit()
		}
	}
	return n, nil
}

// newChunk returns an empty chunk, reusing the buffers of written ones.
func (p *parallelEncoder) newChunk() *chunk {
	c, ok := p.pool.Get().(*chunk)
	if !ok {
		c = &chunk{in: make([]byte, 0, chunkSize)}
	}
	c.in = c.in[:0]
	c.ready = make(chan struct{})
	return c
}

func (p *parallelEncoder) submit() {
	c := p.cur
	p.cur = nil
	p.order <- c
	p.jobs <- c
}

// Close compresses any buffered input, waits for all frames to be written,
// and reports the first error.
func (p *parallelEncoder) Close() error {
	if p.cur != nil && len(p.cur.in) > 0 {
		p.submit()
	}
	close(p.jobs)
	close(p.order)
	<-p.done
	p.wg.Wait()
	p.enc.Close()
	return p.failed()
}
