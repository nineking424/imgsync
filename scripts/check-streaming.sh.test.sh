#!/usr/bin/env bash
# Test for check-streaming.sh: it must catch io.ReadAll under internal/sources/,
# and the bytes.Buffer + io.Copy(buf, body) full-body buffering shape.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

mkdir -p "$TMPDIR/internal/sources/bad"
cat > "$TMPDIR/internal/sources/bad/source.go" <<'EOF'
package bad

import (
	"context"
	"io"
	"os"
)

func Open(_ context.Context, p string) ([]byte, error) {
	f, _ := os.Open(p)
	return io.ReadAll(f)
}
EOF

cd "$TMPDIR"
if "$REPO_ROOT/scripts/check-streaming.sh"; then
  echo "FAIL: streaming guard did not detect io.ReadAll" >&2
  exit 1
fi
echo "PASS: streaming guard rejected io.ReadAll fixture"

# Regression: bytes.Buffer accumulation + io.Copy into the buffer drains the
# whole body into RAM but never names the token "body" next to bytes.NewBuffer,
# so the original regex let it through (issue #26). The guard MUST reject it.
TMPDIR2=$(mktemp -d)
trap 'rm -rf "$TMPDIR" "$TMPDIR2"' EXIT

mkdir -p "$TMPDIR2/internal/transports/buf"
cat > "$TMPDIR2/internal/transports/buf/transport.go" <<'EOF'
package buf

import (
	"bytes"
	"context"
	"io"
)

func Send(_ context.Context, dst string, body io.Reader) error {
	buf := &bytes.Buffer{}
	if _, err := io.Copy(buf, body); err != nil {
		return err
	}
	return store(dst, buf)
}

func store(_ string, _ *bytes.Buffer) error { return nil }
EOF

cd "$TMPDIR2"
if "$REPO_ROOT/scripts/check-streaming.sh"; then
  echo "FAIL: streaming guard did not detect bytes.Buffer + io.Copy(buf, body)" >&2
  exit 1
fi
echo "PASS: streaming guard rejected bytes.Buffer + io.Copy fixture"
