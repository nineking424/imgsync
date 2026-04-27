#!/usr/bin/env bash
# Test for check-streaming.sh: it must catch io.ReadAll under internal/sources/.
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
echo "PASS: streaming guard rejected bad fixture"
