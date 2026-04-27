package localfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/nineking424/imgsync/internal/transfer"
)

// Source reads files from the local filesystem. Suitable for tests and as a
// reference implementation of the streaming Source contract.
type Source struct{}

// NewSource constructs a LocalFS Source.
func NewSource() *Source { return &Source{} }

// Open returns an os.File handle. The caller is responsible for Close().
func (s *Source) Open(ctx context.Context, src string) (io.ReadCloser, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	st, err := os.Stat(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, fmt.Errorf("localfs: stat %s: %w", src, transfer.ErrPermanent)
		}
		return nil, 0, fmt.Errorf("localfs: stat %s: %w", src, err)
	}
	if st.IsDir() {
		return nil, 0, fmt.Errorf("localfs: %s is a directory: %w", src, transfer.ErrPermanent)
	}
	f, err := os.Open(src)
	if err != nil {
		return nil, 0, fmt.Errorf("localfs: open %s: %w", src, err)
	}
	return f, st.Size(), nil
}
