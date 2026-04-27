package transfer

import "errors"

// ErrSkippable signals the worker to mark the job 'skipped' (terminal, audit-only).
// Use for D6 size mismatch, dst-already-exists with identical sha256, etc.
var ErrSkippable = errors.New("skippable: job intentionally not transferred, audit only")

// ErrPermanent signals the worker to mark the job 'dead' immediately, bypassing
// the retry budget. Use for malformed src URI, auth failures, or any error that
// will not change with another attempt.
var ErrPermanent = errors.New("permanent: do not retry, mark dead")
