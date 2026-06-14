package env_test

import (
	"testing"

	"github.com/nineking424/imgsync/internal/env"
)

// These tests pin the behavior the shared internal/env helper must expose so the
// duplicate envInt (cmd/imgsync/worker.go + internal/cli/sniffer.go) and the
// cli-only envBool can be collapsed onto a single parse-failure policy. RED until
// internal/env exists.

func TestInt_AbsentReturnsDefault(t *testing.T) {
	t.Setenv("IMGSYNC_TEST_INT", "")
	// empty/unset must fall back to def
	if got := env.Int("IMGSYNC_TEST_INT_UNSET", 7); got != 7 {
		t.Fatalf("Int(unset) = %d, want 7", got)
	}
}

func TestInt_ValidParses(t *testing.T) {
	t.Setenv("IMGSYNC_TEST_INT", "42")
	if got := env.Int("IMGSYNC_TEST_INT", 7); got != 42 {
		t.Fatalf("Int(42) = %d, want 42", got)
	}
}

func TestInt_InvalidReturnsDefault(t *testing.T) {
	// Parse-failure policy: a malformed value must fall back to the default,
	// matching both prior call sites (worker warned, sniffer was silent — the
	// shared helper keeps the fall-back-to-default semantics).
	t.Setenv("IMGSYNC_TEST_INT", "not-an-int")
	if got := env.Int("IMGSYNC_TEST_INT", 7); got != 7 {
		t.Fatalf("Int(invalid) = %d, want 7 (fall back to default)", got)
	}
}

func TestBool_AbsentReturnsDefault(t *testing.T) {
	if got := env.Bool("IMGSYNC_TEST_BOOL_UNSET", true); got != true {
		t.Fatalf("Bool(unset,true) = %v, want true", got)
	}
	if got := env.Bool("IMGSYNC_TEST_BOOL_UNSET", false); got != false {
		t.Fatalf("Bool(unset,false) = %v, want false", got)
	}
}

func TestBool_TruthyAndFalsy(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"True", true},
		{"0", false},
		{"false", false},
		{"anything-else", false},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("IMGSYNC_TEST_BOOL", tc.val)
			if got := env.Bool("IMGSYNC_TEST_BOOL", true); got != tc.want {
				t.Fatalf("Bool(%q) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}
