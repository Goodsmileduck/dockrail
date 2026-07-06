package engine

import "strings"

// shQuote wraps a string in single quotes for safe use as one POSIX shell word,
// escaping any embedded single quotes via the '\” idiom. Use it for every
// host-sourced or operator-supplied value interpolated into a shell command.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
