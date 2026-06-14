package worker

import (
	"strings"
	"testing"
)

// These tests pin the issue-#37 requirement that error text stored into
// transfer_events.detail is (1) length-bounded and (2) credential-scrubbed before
// it lands in the audit log. RED until a sanitizeErrMsg helper exists and is used
// by openErrDetails / transportErrDetails.

func TestSanitizeErrMsg_ScrubsFTPCredentials(t *testing.T) {
	in := "ftp source: retr ftp://alice:s3cr3t@ftp.example.com/path/file.bin: connection reset"
	out := sanitizeErrMsg(in)
	if strings.Contains(out, "s3cr3t") {
		t.Fatalf("password leaked into detail: %q", out)
	}
	if strings.Contains(out, "alice:s3cr3t") {
		t.Fatalf("userinfo leaked into detail: %q", out)
	}
	// The host/path context should survive so the message stays useful.
	if !strings.Contains(out, "ftp.example.com") {
		t.Fatalf("host context lost during scrub: %q", out)
	}
}

func TestSanitizeErrMsg_ScrubsUserOnlyCredentials(t *testing.T) {
	// userinfo without a colon (user, no password) must still be removed.
	in := "dial ftp://serviceacct@ftp.internal/x: timeout"
	out := sanitizeErrMsg(in)
	if strings.Contains(out, "serviceacct@") {
		t.Fatalf("username userinfo leaked into detail: %q", out)
	}
}

func TestSanitizeErrMsg_TruncatesLongMessages(t *testing.T) {
	long := strings.Repeat("x", 8000)
	out := sanitizeErrMsg(long)
	if len(out) >= len(long) {
		t.Fatalf("long message not truncated: got len %d", len(out))
	}
	if len(out) > 1024 {
		t.Fatalf("truncated message still too long: len %d, want <= 1024", len(out))
	}
}

func TestSanitizeErrMsg_ShortMessageUnchanged(t *testing.T) {
	in := "no such file or directory"
	if out := sanitizeErrMsg(in); out != in {
		t.Fatalf("short, credential-free message altered: %q -> %q", in, out)
	}
}
