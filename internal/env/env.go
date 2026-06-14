// Package env provides small typed accessors for process environment variables
// with a single, consistent parse-failure policy: an absent, empty, or malformed
// value falls back to the supplied default.
package env

import (
	"os"
	"strconv"
	"strings"
)

// Int returns the integer value of key, or def if the variable is absent, empty,
// or cannot be parsed as an integer.
func Int(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// Bool returns the boolean value of key, or def if the variable is absent or
// empty. A present value is true only when it is "1" or "true" (case-insensitive);
// every other non-empty value is false.
func Bool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v == "1" || strings.EqualFold(v, "true")
}
