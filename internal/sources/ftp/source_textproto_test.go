package ftp_test

import (
	"errors"
	"fmt"
	"net/textproto"
	"testing"

	srcftp "github.com/nineking424/imgsync/internal/sources/ftp"
	"github.com/stretchr/testify/require"
)

// These tests pin the issue-#37 requirement that the FTP source's not-found
// classifier keys off the jlaffaye *textproto.Error reply *code* (550 =
// StatusFileUnavailable), NOT substrings of the message text — matching the
// precedent already set in internal/transports/ftp/transport.go:classify. They
// are RED while isNotFound still does strings.Contains on err.Error().

// A 550 reply with a non-English message (no "no such file"/"not found"/etc.
// substring) must still be classified as not-found via the reply code.
func TestFTPSource_isNotFound_550_NonEnglishMessage(t *testing.T) {
	err := &textproto.Error{Code: 550, Msg: "Datei nicht gefunden"} // German: file not found
	require.True(t, srcftp.IsNotFoundForTest(err),
		"550 reply must classify as not-found by code, regardless of message locale")
}

// A wrapped 550 (errors.As must unwrap through %w) is still not-found.
func TestFTPSource_isNotFound_550_Wrapped(t *testing.T) {
	base := &textproto.Error{Code: 550, Msg: "Requested action not taken"}
	wrapped := fmt.Errorf("ftp retr: %w", base)
	require.True(t, srcftp.IsNotFoundForTest(wrapped),
		"550 reply wrapped with %%w must still classify as not-found")
}

// A non-550 reply code must NOT be treated as not-found even if its message text
// happens to contain a missing-file keyword (string matcher would false-positive).
func TestFTPSource_isNotFound_Non550_NotFound(t *testing.T) {
	err := &textproto.Error{Code: 530, Msg: "Login incorrect: file not found in vault"}
	require.False(t, srcftp.IsNotFoundForTest(err),
		"non-550 reply code must not be classified as not-found, even with a keyword in the text")
}

// A plain (non-textproto) error must not be classified as not-found: without a
// reply code there is no protocol signal to treat the file as missing.
func TestFTPSource_isNotFound_PlainError_NotFound(t *testing.T) {
	require.False(t, srcftp.IsNotFoundForTest(errors.New("dial tcp: connection refused")),
		"a non-protocol error must not be classified as not-found")
}

// A 550 reply carrying a permission / access-denied message is an operator
// misconfiguration, NOT a skippable missing source: it must surface (retry then
// dead), so isNotFound carves it out by message and returns false.
func TestFTPSource_isNotFound_550_PermissionDenied_NotSkippable(t *testing.T) {
	for _, msg := range []string{"Permission denied", "Access denied", "550 permission denied"} {
		err := &textproto.Error{Code: 550, Msg: msg}
		require.False(t, srcftp.IsNotFoundForTest(err),
			"550 permission/access-denied (%q) must not be classified as skippable not-found", msg)
	}
}
