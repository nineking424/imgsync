package transfer_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/nineking424/imgsync/internal/transfer"
	"github.com/stretchr/testify/require"
)

func TestErrSkippable_WrapsAndUnwraps(t *testing.T) {
	wrapped := fmt.Errorf("size mismatch: %w", transfer.ErrSkippable)
	require.True(t, errors.Is(wrapped, transfer.ErrSkippable))
	require.False(t, errors.Is(wrapped, transfer.ErrPermanent))
}

func TestErrPermanent_WrapsAndUnwraps(t *testing.T) {
	wrapped := fmt.Errorf("auth failed: %w", transfer.ErrPermanent)
	require.True(t, errors.Is(wrapped, transfer.ErrPermanent))
	require.False(t, errors.Is(wrapped, transfer.ErrSkippable))
}

func TestErrSentinels_AreDistinct(t *testing.T) {
	require.NotEqual(t, transfer.ErrSkippable, transfer.ErrPermanent)
}
