// helpers.go — Small utility functions shared across the package.
package nuts

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// toJSON marshals any value to a JSON string. On error it returns "{}".
// Used primarily to embed payloads inside SSE data lines.
func toJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// tryParseJSON attempts to preserve raw JSON values without coercing numbers
// through interface{} / float64. If the bytes are valid JSON, a compacted
// json.RawMessage is returned so json.Marshal embeds it directly in the SSE
// envelope. Otherwise the raw bytes are returned as a plain string.
//
// Callers MUST bound len(data) before invoking this function — JSON compaction
// allocates and is unsafe on untrusted unbounded input.
func tryParseJSON(data []byte) interface{} {
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, data); err != nil {
		return string(data)
	}
	return json.RawMessage(compacted.Bytes())
}

// writeSSEChunk writes a complete SSE frame to the client and flushes it.
func writeSSEChunk(w io.Writer, flusher http.Flusher, chunk string) error {
	if _, err := io.WriteString(w, chunk); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func writeSSEChunkWithTimeout(w http.ResponseWriter, flusher http.Flusher, chunk string, timeout time.Duration) error {
	if timeout <= 0 {
		return writeSSEChunk(w, flusher, chunk)
	}

	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		if !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		return writeSSEChunk(w, flusher, chunk)
	}

	if err := writeSSEChunk(w, flusher, chunk); err != nil {
		return err
	}
	if err := controller.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

// isAllowedTopicByte reports whether c may appear in a topic name. Accepted
// characters: ASCII letters, digits, dot, dash, underscore.
func isAllowedTopicByte(c byte) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '.' || c == '-' || c == '_'
}

// isValidTopic rejects topic names that would be problematic as NATS
// subjects. Accepted character set: ASCII letters, digits, dot, dash,
// underscore. Rejects wildcards (* and >), the system prefix ($),
// leading/trailing/consecutive dots, and any length over 256 bytes.
func isValidTopic(topic string) bool {
	const maxTopicLen = 256
	if topic == "" || len(topic) > maxTopicLen {
		return false
	}
	if strings.HasPrefix(topic, "$") {
		return false
	}
	if strings.Contains(topic, "..") {
		return false
	}
	if strings.HasPrefix(topic, ".") || strings.HasSuffix(topic, ".") {
		return false
	}
	for i := 0; i < len(topic); i++ {
		if !isAllowedTopicByte(topic[i]) {
			return false
		}
	}
	return true
}

// redactURL strips embedded credentials from a URL string before it is
// written to logs.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User("REDACTED")
	return u.String()
}

// isAllowedCookieNameByte reports whether c may appear in a cookie name per
// RFC 6265's token rules: ASCII letters, digits, and the RFC's permitted
// punctuation set.
func isAllowedCookieNameByte(c byte) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c))
}

func isValidCookieName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !isAllowedCookieNameByte(name[i]) {
			return false
		}
	}
	return true
}
