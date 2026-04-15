// helpers.go — Small utility functions shared across the package.
//
// These helpers handle JSON serialization, SSE wire-format writing,
// topic name validation, and URL redaction for safe logging.
package nuts

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode"
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
// Otherwise the raw bytes are returned as a plain string. This lets
// messageEventPayload.Payload stay structured when possible.
func tryParseJSON(data []byte) interface{} {
	var v interface{}
	if err := json.Unmarshal(data, &v); err != nil {
		return string(data)
	}
	return v
}

// writeSSEChunk writes a complete SSE frame to the client and flushes it.
// The SSE protocol requires each event to end with a blank line (\n\n).
// Flushing after every chunk ensures the browser receives the data
// immediately instead of waiting for the server's write buffer to fill.
func writeSSEChunk(w io.Writer, flusher http.Flusher, chunk string) error {
	if _, err := io.WriteString(w, chunk); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// isValidTopic rejects topic names that would be problematic as NATS
// subjects: empty strings, overly long names, wildcard characters,
// system-reserved prefixes, control characters, or empty segments.
func isValidTopic(topic string) bool {
	// maxTopicLen caps how long a topic name can be. This prevents
	// clients from submitting absurdly long subjects that could waste
	// memory or cause issues with NATS subject limits.
	const maxTopicLen = 256
	if topic == "" || len(topic) > maxTopicLen {
		return false
	}
	// Reject NATS wildcards (* and >). Server-side consumers should use
	// TopicPrefix for broad subscriptions; allowing wildcards here would
	// let any browser client subscribe to arbitrary subject patterns.
	if strings.ContainsAny(topic, "*>") {
		return false
	}
	// Reject subjects starting with $ — these are reserved by NATS for
	// internal use (e.g., $JS.API for JetStream, $SYS for system events).
	if strings.HasPrefix(topic, "$") {
		return false
	}
	// Reject control characters (ASCII 0x00-0x1F and 0x7F). These can break
	// the SSE wire format or cause unexpected behaviour in NATS subjects.
	for _, r := range topic {
		if unicode.IsControl(r) {
			return false
		}
	}
	// Reject ".." (consecutive dots). In NATS, dots separate subject tokens,
	// so ".." means an empty token, which is invalid.
	if strings.Contains(topic, "..") {
		return false
	}
	return true
}

// redactURL strips embedded credentials from a URL string before it is
// written to logs. Without this, a NATS URL like "nats://user:pass@host"
// would leak the password into Caddy's log output.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User("REDACTED")
	return u.String()
}
