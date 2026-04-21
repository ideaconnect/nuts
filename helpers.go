// helpers.go — Small utility functions shared across the package.
package nuts

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
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

// tryParseJSON attempts to unmarshal raw bytes as JSON. If the bytes are
// valid JSON, the parsed value is returned (map, slice, number, etc.).
// Otherwise the raw bytes are returned as a plain string.
//
// Callers MUST bound len(data) before invoking this function — JSON
// unmarshalling allocates and is unsafe on untrusted unbounded input.
func tryParseJSON(data []byte) interface{} {
	var v interface{}
	if err := json.Unmarshal(data, &v); err != nil {
		return string(data)
	}
	return v
}

// writeSSEChunk writes a complete SSE frame to the client and flushes it.
func writeSSEChunk(w io.Writer, flusher http.Flusher, chunk string) error {
	if _, err := io.WriteString(w, chunk); err != nil {
		return err
	}
	flusher.Flush()
	return nil
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
		c := topic[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '.' || c == '-' || c == '_':
		default:
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
