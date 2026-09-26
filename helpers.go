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

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// nopLogger is reused by Handler.log() when h.logger is nil so call sites
// don't have to spell out the guard each time. zap.NewNop() is cheap to
// construct but reusing one instance keeps the hot path branch-free.
var nopLogger = zap.NewNop()

// maxSubjectLen caps the byte length of an accepted NATS subject string —
// applied to both request topics (isValidTopic) and JWT-claim filters
// (isValidTopicFilter in auth.go). The two validators differ in alphabet
// and wildcard rules, but the length contract is the same; hoisting the
// constant prevents the two sides from drifting if it ever needs raising.
const maxSubjectLen = 256

// log returns h.logger when set and a no-op logger otherwise. Keeps every
// h.log().X(...) call site safe regardless of whether the handler went
// through Provision (which sets h.logger from ctx.Logger) or was
// constructed directly by a test that forgot to set the field.
func (h *Handler) log() *zap.Logger {
	if h.logger != nil {
		return h.logger
	}
	return nopLogger
}

// classifyNATSAsyncError maps a nats.go async error to a stable Prometheus
// label value. Keep the label set small and bounded — Prometheus cardinality
// matters, and operators triage on kind, not the underlying error string.
//
// Label set (must stay in sync with the README and OPERATIONS.md
// documented values): slow_consumer, timeout, connection_state,
// consumer_invalidated, other. The caller (provision.go's
// nats.ErrorHandler) filters nil errors before calling, so this function
// does not return an "unknown" or similar label that would drift outside
// the documented set.
//
// consumer_invalidated covers three distinct nats.go error paths that
// all indicate the JetStream push consumer is no longer usable:
//
//   - nats.ErrConsumerNotActive (sentinel, errors.Is): raised by
//     nats.go's activityCheck() when no IdleHeartbeat has arrived
//     within the configured tolerance. This is the primary failure
//     mode M9 targets — the server reaped or lost track of the
//     ephemeral, e.g. InactiveThreshold tripped during a network
//     blip or a leafnode route failover dropped the inbox.
//   - nats.ErrConsumerDeleted (sentinel, errors.Is): raised when the
//     consumer was administratively deleted while a subscription
//     held it. In the pinned nats.go v1.52.0 legacy `nats` package
//     this sentinel is only dispatched from the pull-request 409
//     status handler (js.go:checkMsg → jetStream409Sts) — push
//     subscribers like NUTS will not see it via AsyncErrorCB on this
//     library version (an admin-delete of a push consumer surfaces
//     via ErrConsumerNotActive once heartbeats stop). The arm is
//     kept as defensive forward-compat against (a) the newer
//     github.com/nats-io/nats.go/jetstream subpackage, which DOES
//     dispatch this sentinel on push paths, and (b) future legacy-
//     package changes that may unify the push and pull dispatch
//     sites.
//   - *nats.ErrConsumerSequenceMismatch (typed struct, errors.As):
//     raised by checkForSequenceMismatch when heartbeats DO arrive
//     but the delivered consumer sequence has drifted from the
//     value carried in the heartbeat — interrupts ordered replay.
//     Mirrors serve.go:306 isReplayStartSequenceError's typed-error
//     pattern.
//
// M9 Batch B will route this label as the trigger for terminating the
// affected SSE handler so the client reconnects with Last-Event-ID
// against a fresh consumer; today (Batch A) it is observability-only.
//
// The mix of errors.Is and errors.As is load-bearing. ErrConsumerNot
// Active and ErrConsumerDeleted are JetStreamError-interface values
// backed by *jsError sentinels — errors.Is is the right matcher.
// ErrConsumerSequenceMismatch is a struct type carrying recovery
// sequence numbers — errors.As is required because errors.Is would
// compare against a zero-value sentinel and silently fall through to
// "other". The helpers_test.go table pins both contracts explicitly.
func classifyNATSAsyncError(err error) string {
	var sequenceMismatch *nats.ErrConsumerSequenceMismatch
	switch {
	case errors.Is(err, nats.ErrSlowConsumer):
		return "slow_consumer"
	case errors.Is(err, nats.ErrTimeout):
		return "timeout"
	case errors.Is(err, nats.ErrConnectionClosed), errors.Is(err, nats.ErrConnectionDraining):
		return "connection_state"
	case errors.Is(err, nats.ErrConsumerNotActive),
		errors.Is(err, nats.ErrConsumerDeleted),
		errors.As(err, &sequenceMismatch):
		return "consumer_invalidated"
	default:
		return "other"
	}
}

// toJSON marshals any value to a JSON string. On error it returns "{}".
// Used primarily to embed payloads inside SSE data lines.
func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// writeJSONString writes s as a JSON string, escaped exactly as json.Marshal
// escapes it, HTML characters included. Printable ASCII without characters
// that need escaping, which covers every topic, skips the encoder.
func writeJSONString(b *strings.Builder, s string) {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c > 0x7e || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			encoded, _ := json.Marshal(s) // a string always encodes
			b.Write(encoded)
			return
		}
	}
	b.WriteByte('"')
	b.WriteString(s)
	b.WriteByte('"')
}

// lineSeparatorTail and paragraphSeparatorTail follow the 0xE2 lead byte in
// the UTF-8 encodings of U+2028 and U+2029, which json.Marshal escapes.
var (
	lineSeparatorTail      = []byte{0x80, 0xa8}
	paragraphSeparatorTail = []byte{0x80, 0xa9}
)

// writeJSONPayload writes a message payload into the frame's JSON envelope as
// json.Marshal would: valid JSON compacted and HTML-escaped as for a
// json.RawMessage, anything else as a JSON string. Valid JSON is copied in
// one pass: whitespace outside strings is dropped, and '<', '>', '&', U+2028
// and U+2029, which valid JSON can only hold inside strings, are escaped.
func writeJSONPayload(b *strings.Builder, data []byte) {
	if !json.Valid(data) {
		writeJSONString(b, string(data))
		return
	}
	const hex = "0123456789abcdef"
	start := 0 // first byte not yet written
	inString := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if !inString {
			switch c {
			case '"':
				inString = true
			case ' ', '\t', '\n', '\r':
				b.Write(data[start:i])
				start = i + 1
			}
			continue
		}
		switch {
		case c == '\\':
			i++ // the escaped byte cannot end the string or need escaping
		case c == '"':
			inString = false
		case c == '<' || c == '>' || c == '&':
			b.Write(data[start:i])
			b.WriteString(`\u00`)
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
			start = i + 1
		case c == 0xe2 && (bytes.HasPrefix(data[i+1:], lineSeparatorTail) || bytes.HasPrefix(data[i+1:], paragraphSeparatorTail)):
			b.Write(data[start:i])
			b.WriteString(`\u202`)
			b.WriteByte(hex[data[i+2]&0xf])
			i += 2
			start = i + 1
		}
	}
	b.Write(data[start:])
}

// supportsFlush reports whether w, or any writer it wraps via Unwrap, can
// flush. It mirrors http.ResponseController's lookup: Caddy wraps the writer
// in a recorder that implements FlushError and Unwrap but not http.Flusher
// whenever access logging or HTTP metrics are enabled, so a plain
// w.(http.Flusher) assertion wrongly rejects every stream.
func supportsFlush(w http.ResponseWriter) bool {
	for w != nil {
		switch w.(type) {
		case interface{ FlushError() error }, http.Flusher:
			return true
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		w = unwrapper.Unwrap()
	}
	return false
}

// writeSSEChunk writes a complete SSE frame and flushes it. The flush error is
// returned so a frame that never reached the client is not counted as sent.
func writeSSEChunk(w io.Writer, rc *http.ResponseController, chunk string) error {
	if _, err := io.WriteString(w, chunk); err != nil {
		return err
	}
	return rc.Flush()
}

func writeSSEChunkWithTimeout(w http.ResponseWriter, rc *http.ResponseController, chunk string, timeout time.Duration) error {
	return writeSSEChunksWithTimeout(w, rc, timeout, chunk)
}

// writeSSEChunksWithTimeout writes frames and flushes them once, under one
// write deadline when timeout is positive. The deadline is cleared again
// afterwards: on HTTP/2 a deadline that expires while the stream is idle
// resets it.
func writeSSEChunksWithTimeout(w http.ResponseWriter, rc *http.ResponseController, timeout time.Duration, chunks ...string) error {
	armed := false
	if timeout > 0 {
		err := rc.SetWriteDeadline(time.Now().Add(timeout))
		if err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		armed = err == nil
	}
	for _, chunk := range chunks {
		if _, err := io.WriteString(w, chunk); err != nil {
			return err
		}
	}
	if err := rc.Flush(); err != nil {
		return err
	}
	if armed {
		if err := rc.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
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
	if topic == "" || len(topic) > maxSubjectLen {
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

// redactURL strips embedded credentials from a URL string, or from each
// server of a comma-separated list as nats_url accepts, before it is written
// to logs.
func redactURL(raw string) string {
	servers := strings.Split(raw, ",")
	for i, server := range servers {
		servers[i] = redactServerURL(server)
	}
	return strings.Join(servers, ",")
}

// redactServerURL redacts the user information of one server URL. A server
// without a scheme is parsed as nats://, as nats.go dials it; otherwise
// "user:pass@host" would parse as scheme "user" and keep the password. In a
// URL that does not parse, everything up to the last '@' is redacted.
func redactServerURL(server string) string {
	trimmed := strings.TrimSpace(server)
	u, err := url.Parse(withNATSScheme(trimmed))
	if err != nil {
		at := strings.LastIndex(trimmed, "@")
		if at < 0 {
			return server
		}
		scheme := ""
		if i := strings.Index(trimmed, "://"); i >= 0 && i < at {
			scheme = trimmed[:i+3]
		}
		return scheme + "REDACTED" + trimmed[at:]
	}
	if u.User == nil {
		return server
	}
	u.User = url.User("REDACTED")
	if !strings.Contains(trimmed, "://") {
		return strings.TrimPrefix(u.String(), "nats://")
	}
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
