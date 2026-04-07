package nuts

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode"
)

// toJSON converts a value to JSON string.
func toJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// tryParseJSON attempts to parse data as JSON, returns string if not valid JSON.
func tryParseJSON(data []byte) interface{} {
	var v interface{}
	if err := json.Unmarshal(data, &v); err != nil {
		return string(data)
	}
	return v
}

// writeSSEChunk writes one complete SSE chunk and flushes it to the client.
func writeSSEChunk(w io.Writer, flusher http.Flusher, chunk string) error {
	if _, err := io.WriteString(w, chunk); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// isValidTopic rejects topic names that contain control characters, empty
// segments, NATS wildcards, or NATS internal subjects.
func isValidTopic(topic string) bool {
	const maxTopicLen = 256
	if topic == "" || len(topic) > maxTopicLen {
		return false
	}
	// Reject NATS wildcards — server-side consumers should use TopicPrefix for broad subscriptions.
	if strings.ContainsAny(topic, "*>") {
		return false
	}
	// Reject NATS internal/system subjects (e.g. $JS, $SYS).
	if strings.HasPrefix(topic, "$") {
		return false
	}
	for _, r := range topic {
		if unicode.IsControl(r) {
			return false
		}
	}
	if strings.Contains(topic, "..") {
		return false
	}
	return true
}

// redactURL strips userinfo (embedded credentials / tokens) from a URL before logging.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User("REDACTED")
	return u.String()
}
