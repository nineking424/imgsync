package worker

import (
	"math/rand"
	"regexp"
	"sync"
	"time"
)

// maxDetailLen bounds the error text stored into transfer_events.detail so a
// pathological error message cannot bloat the audit log.
const maxDetailLen = 1024

// credRe matches a URL userinfo component (scheme://user[:pass]@) so credentials
// embedded in an error string are scrubbed before the message is persisted. The
// host/path that follows the "@" is preserved for diagnostic context.
var credRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@\s]+@`)

// sanitizeErrMsg scrubs embedded URL credentials and bounds the length of an
// error message before it is written to transfer_events.detail. A short,
// credential-free message is returned unchanged.
func sanitizeErrMsg(s string) string {
	s = credRe.ReplaceAllString(s, "$1")
	if len(s) > maxDetailLen {
		s = s[:maxDetailLen]
	}
	return s
}

var (
	backoffMu  sync.Mutex
	backoffRNG = rand.New(rand.NewSource(time.Now().UnixNano()))
)

// retryBackoff returns the retry delay for the given attempt count: the nominal
// (1<<attempts) seconds with ±25% uniform jitter applied so retries across the
// fleet do not synchronize into thundering-herd waves.
func retryBackoff(attempts int) time.Duration {
	nominal := float64(int64(1)<<attempts) * float64(time.Second)
	backoffMu.Lock()
	offset := (backoffRNG.Float64() - 0.5) * 0.5 * nominal // ±25%
	backoffMu.Unlock()
	return time.Duration(nominal + offset)
}
