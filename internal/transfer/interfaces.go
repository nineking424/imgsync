package transfer

import (
	"context"
	"io"
)

// Source opens a streaming reader for src. The caller MUST Close the returned
// ReadCloser. Implementations MUST NOT buffer the body in memory.
//
// Returned size is the source's reported byte count, or -1 if unknown.
type Source interface {
	Open(ctx context.Context, src string) (body io.ReadCloser, size int64, err error)
}

// Transport streams body to dst. expectedSize is the source's reported byte count
// (-1 if unknown). Implementations MUST count bytes actually written and compute
// sha256 over the streamed bytes; the worker uses these for D6 size verification.
//
// Implementations MUST NOT buffer the entire body in memory.
type Transport interface {
	Send(
		ctx context.Context,
		dst string,
		body io.Reader,
		expectedSize int64,
	) (writtenBytes int64, sha256Hex string, err error)
}
