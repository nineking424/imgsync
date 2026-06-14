package transfer

import (
	"context"
	"io"
)

// ctxReader wraps an io.Reader so that Read unblocks and returns ctx.Err() when
// ctx is cancelled, even if the underlying Read blocks indefinitely. It runs the
// underlying Read in a goroutine and races it against ctx.Done(), so an in-flight
// transfer (io.Copy) aborts promptly on cancellation. It does not buffer the full
// body: the goroutine reads into a single bounded scratch buffer, one underlying
// Read at a time, and the bytes are copied out to the caller.
type ctxReader struct {
	ctx     context.Context
	r       io.Reader
	scratch []byte          // owned by the reader goroutine; never shared with the caller's p
	pending chan readResult // carries the result of an in-flight underlying Read
}

type readResult struct {
	n   int
	err error
}

// NewCtxReader returns an io.Reader that honors ctx cancellation on Read. A nil
// or already-cancelled ctx makes Read return ctx.Err() immediately. The returned
// reader is not safe for concurrent use.
func NewCtxReader(ctx context.Context, r io.Reader) io.Reader {
	return &ctxReader{ctx: ctx, r: r}
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	// A prior Read may have been abandoned mid-flight on cancellation. Once a
	// goroutine is reading into the scratch buffer we must not start another
	// concurrent Read, so reuse the pending channel: the abandoned goroutine
	// eventually delivers its result here. The goroutine reads into c.scratch
	// (which it solely owns until it sends), so it never races the caller's p.
	if c.pending == nil {
		if len(c.scratch) < len(p) {
			c.scratch = make([]byte, len(p))
		}
		c.pending = make(chan readResult, 1)
		go func() {
			n, err := c.r.Read(c.scratch[:len(p)])
			c.pending <- readResult{n: n, err: err}
		}()
	}

	select {
	case res := <-c.pending:
		c.pending = nil
		n := copy(p, c.scratch[:res.n])
		return n, res.err
	case <-c.ctx.Done():
		// Leave c.pending in place; the goroutine still owns c.scratch. The caller
		// stops on this error, so neither p nor c.scratch is touched concurrently.
		return 0, c.ctx.Err()
	}
}
