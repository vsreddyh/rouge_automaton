// Package httpx holds the small helpers the API clients share: reading a
// server's error body into a line worth logging.
package httpx

import "strings"

// maxSnippet caps the error body a log line carries.
const maxSnippet = 200

// BodySnippet renders an error body as a short, single-line hint: a proxy
// or a gateway says why in the body, and the reason is worth the log
// line. An empty body reads empty.
func BodySnippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > maxSnippet {
		s = s[:maxSnippet]
	}
	return s
}

// BodySuffix renders an error body as the tail of a log line: ": why",
// or nothing at all when the body is empty.
func BodySuffix(body []byte) string {
	s := BodySnippet(body)
	if s == "" {
		return ""
	}
	return ": " + s
}
