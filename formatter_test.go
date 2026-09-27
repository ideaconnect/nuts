package nuts

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// referenceMessageFrame renders a message frame the way formatMessageEvent
// did before it was rewritten as a single pass (#122): compact the payload,
// wrap it in messageEventPayload and json.Marshal the envelope. The rewrite
// must stay byte-identical to it, since clients may depend on the exact
// escaping. (The id line moved last separately, #107.)
func referenceMessageFrame(h *Handler, msg streamMessage, now time.Time) string {
	payload := messageEventPayload{
		Topic:   strings.TrimPrefix(msg.Subject, h.TopicPrefix),
		Payload: tryParseJSON(msg.Data),
	}
	if msg.HasMetadata {
		payload.Time = msg.Timestamp.UTC().Format(time.RFC3339)
	} else {
		payload.Time = now.UTC().Format(time.RFC3339)
	}
	var event strings.Builder
	event.WriteString("event: " + referenceEventName(h, msg) + "\n")
	if h.PayloadFormat == "raw" {
		text := string(msg.Data)
		if !utf8.ValidString(text) || strings.ContainsRune(text, '\r') {
			return ""
		}
		for _, line := range strings.Split(text, "\n") {
			event.WriteString("data: " + line + "\n")
		}
	} else {
		event.WriteString("data: ")
		event.WriteString(toJSON(payload))
		event.WriteString("\n")
	}
	if msg.HasMetadata {
		event.WriteString("id: ")
		event.WriteString(strconv.FormatUint(msg.StreamSequence, 10))
		if h.EventIDFormat == eventIDSequenceTime {
			event.WriteString("-" + strconv.FormatInt(msg.Timestamp.UnixNano(), 10))
		}
		event.WriteString("\n")
	}
	event.WriteString("\n")
	return event.String()
}

// referenceEventName is the reference formatter's event_type step, written
// with a regular expression rather than the handler's byte loop.
var referenceEventNamePattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,100}$`)

func referenceEventName(h *Handler, msg streamMessage) string {
	var name string
	switch h.EventType {
	case "topic":
		name = strings.TrimPrefix(msg.Subject, h.TopicPrefix)
	case "header":
		name = msg.Header.Get(h.EventTypeHeader)
	default:
		return "message"
	}
	switch {
	case !referenceEventNamePattern.MatchString(name), name == "connected", name == "reset", name == "open", name == "error":
		return "message"
	}
	return name
}

// tryParseJSON is the reference formatter's payload step: valid JSON is
// compacted into a json.RawMessage, anything else is kept as a string.
func tryParseJSON(data []byte) any {
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, data); err != nil {
		return string(data)
	}
	return json.RawMessage(compacted.Bytes())
}

// formatterEdgeCases are payloads whose encoding is easy to get subtly
// wrong: HTML characters, U+2028/2029, escapes, whitespace, non-JSON input,
// invalid UTF-8 and numbers that must not be reformatted.
var formatterEdgeCases = []string{
	`{"kind":"bench","value":123,"nested":{"ok":true}}`,
	` { "a" : [ 1 , 2 ] ,"b":{ } } `,
	"{\n\t\"a\": 1\r\n}",
	`{"html":"<b>Tom & Jerry</b>"}`,
	"{\"ls\":\"a\u2028b\u2029c\"}",
	`{"escaped":"\"quoted\" \\ \/ \b \f \n \r \t \u2028 \u003c"}`,
	`"plain JSON string with <html> & more"`,
	`not JSON <b> & "quotes" \ backslash`,
	"not JSON with a line separator \u2028 and control \x01 characters",
	``,
	`   `,
	`null`,
	`true`,
	`-12.5e+300`,
	`12345678901234567890123456789`,
	`[1,2,3,{"x":[]}]`,
	`{"unicode":"żółć 日本 🎉"}`,
	`{"e2":"… € ‰ ⁄ ₿ ⏎"}`,
	"not JSON … € with \u2028 separators",
	"{\"trunc\":\"\xe2\x80\"}",
	"{\"bad\":\"\xff\xfe\"}",
	"\xff\xfe not UTF-8",
	`{"a":1}{"b":2}`,
	`{"trailing":1,}`,
	strings.Repeat(`{"n":[`, 50) + strings.Repeat(`]}`, 50),
}

func TestFormatMessageEvent_MatchesTheReferenceFormatter(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	for _, format := range []string{eventIDSequence, eventIDSequenceTime} {
		h := &Handler{TopicPrefix: "events.", MaxEventSize: -1, EventIDFormat: format}
		for i, data := range formatterEdgeCases {
			for _, hasMeta := range []bool{true, false} {
				msg := streamMessage{Subject: "events.orders", Data: []byte(data), HasMetadata: hasMeta, StreamSequence: 42, Timestamp: now.Add(-time.Hour)}
				want := referenceMessageFrame(h, msg, now)
				if got := h.formatMessageEvent(msg, now).Frame; got != want {
					t.Errorf("%s case %d (%q, metadata=%v):\n got %q\nwant %q", format, i, data, hasMeta, got, want)
				}
			}
		}
	}
}

func FuzzFormatMessageEventMatchesReference(f *testing.F) {
	for _, data := range formatterEdgeCases {
		f.Add([]byte(data), "orders", uint64(42), true, false, uint8(0), "", false)
		f.Add([]byte(data), "orders", uint64(42), true, true, uint8(1), "", false)
		f.Add([]byte(data), "orders", uint64(42), true, false, uint8(2), "order.created", false)
		f.Add([]byte(data), "orders", uint64(42), true, false, uint8(0), "", true)
	}
	f.Add([]byte(`{}`), "reset", uint64(1), true, false, uint8(1), "", false)
	f.Add([]byte(`{}`), "orders", uint64(1), true, false, uint8(2), "has space", false)
	f.Add([]byte("line one\nline two\n"), "orders", uint64(1), true, false, uint8(0), "", true)
	f.Add([]byte("a\r\nb"), "orders", uint64(1), true, false, uint8(0), "", true)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, data []byte, topic string, seq uint64, hasMeta, timedIDs bool, eventType uint8, header string, raw bool) {
		h := &Handler{TopicPrefix: "events.", MaxEventSize: -1}
		if timedIDs {
			h.EventIDFormat = eventIDSequenceTime
		}
		if raw {
			h.PayloadFormat = payloadFormatRaw
		}
		switch eventType % 3 {
		case 1:
			h.EventType = eventTypeTopic
		case 2:
			h.EventType, h.EventTypeHeader = eventTypeHeader, "Event-Type"
		}
		msg := streamMessage{Subject: "events." + topic, Data: data, Header: nats.Header{"Event-Type": []string{header}}, HasMetadata: hasMeta, StreamSequence: seq, Timestamp: now.Add(-time.Minute)}
		want := referenceMessageFrame(h, msg, now)
		got := h.formatMessageEvent(msg, now).Frame
		if got != want {
			t.Fatalf("frame differs for data=%q topic=%q:\n got %q\nwant %q", data, topic, got, want)
		}
		// #143: what EventSource makes of a raw frame is the payload itself.
		if raw && got != "" {
			if back := eventSourceData(got); back != string(data) {
				t.Fatalf("raw payload %q reads back as %q from %q", data, back, got)
			}
		}
	})
}

// eventSourceData is the data EventSource dispatches for one frame: the
// values of its data lines, joined with "\n".
func eventSourceData(frame string) string {
	var lines []string
	for _, line := range strings.Split(strings.TrimSuffix(frame, "\n\n"), "\n") {
		if value, ok := strings.CutPrefix(line, "data: "); ok {
			lines = append(lines, value)
		}
	}
	return strings.Join(lines, "\n")
}

// TestFormatMessageEvent_RawPayload covers #143: in payload_format raw the
// event's data is the NATS payload, a data line per line of it, which
// EventSource joins back. A payload SSE cannot carry is dropped and counted;
// an oversized one keeps its own reason.
func TestFormatMessageEvent_RawPayload(t *testing.T) {
	h := &Handler{TopicPrefix: "events.", MaxEventSize: -1, PayloadFormat: payloadFormatRaw}
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	frame := func(data string) formattedMessageEvent {
		return h.formatMessageEvent(streamMessage{Subject: "events.orders", Data: []byte(data), HasMetadata: true, StreamSequence: 7, Timestamp: now}, now)
	}
	for data, want := range map[string]string{
		`{"id":1}`:            "event: message\ndata: {\"id\":1}\nid: 7\n\n",
		"line one\nline two":  "event: message\ndata: line one\ndata: line two\nid: 7\n\n",
		"ends with a break\n": "event: message\ndata: ends with a break\ndata: \nid: 7\n\n",
		"":                    "event: message\ndata: \nid: 7\n\n",
		"zażółć <b>&</b>":     "event: message\ndata: zażółć <b>&</b>\nid: 7\n\n",
	} {
		got := frame(data)
		if got.Dropped || got.Frame != want {
			t.Errorf("raw %q: frame %q (dropped %v), want %q", data, got.Frame, got.Dropped, want)
		}
		if back := eventSourceData(got.Frame); back != data {
			t.Errorf("raw %q reads back as %q", data, back)
		}
	}
	for _, data := range []string{"a\r\nb", "carriage\rreturn", "\xff\xfe not UTF-8"} {
		if got := frame(data); !got.Dropped || got.DropReason != dropReasonRawNotText || got.DropSize != len(data) || got.Frame != "" {
			t.Errorf("raw %q: dropped %v for %q (size %d), want a raw_not_text drop", data, got.Dropped, got.DropReason, got.DropSize)
		}
	}
	h.MaxEventSize = 4
	if got := frame("\xff too big"); got.DropReason != dropReasonRawPayload {
		t.Errorf("an oversized payload was dropped for %q, want %q", got.DropReason, dropReasonRawPayload)
	}

	core, obs := observer.New(zap.WarnLevel)
	h.logger = zap.New(core)
	before := metricValue(t, metricsMessagesDropped.WithLabelValues(dropReasonRawNotText))
	h.MaxEventSize = -1
	h.recordDroppedMessage(frame("a\rb"))
	if got := metricValue(t, metricsMessagesDropped.WithLabelValues(dropReasonRawNotText)); got != before+1 {
		t.Fatalf("messages_dropped_total{raw_not_text} = %v, want %v", got, before+1)
	}
	if !hasLogField(obs, "topic", "events.orders") || obs.FilterMessageSnippet("payload_format raw cannot send").Len() != 1 {
		t.Fatalf("the drop was not logged: %+v", obs.All())
	}
}

func BenchmarkFormatMessageEventLarge(b *testing.B) {
	h := &Handler{TopicPrefix: "events.", MaxEventSize: -1}
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	msg := streamMessage{
		Subject:        "events.bench",
		Data:           []byte(`{"blob":"` + strings.Repeat("x", 64*1024) + `","n":[1,2,3]}`),
		HasMetadata:    true,
		StreamSequence: 42,
		Timestamp:      now,
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchmarkFormatted = h.formatMessageEvent(msg, now)
	}
}

func TestWriteJSONPayload(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"key":"value"}`, `{"key":"value"}`},
		{` { "a" : [ 1 , 2 ] } `, `{"a":[1,2]}`},
		{"{\n\t\"a\": 1\r\n}", `{"a":1}`},
		{`{"h":"<b>&</b>"}`, `{"h":"\u003cb\u003e\u0026\u003c/b\u003e"}`},
		{"{\"ls\":\"a\u2028b\u2029c\"}", `{"ls":"a\u2028b\u2029c"}`},
		{`{"q":"\" < "}`, `{"q":"\" \u003c "}`},
		{`{"s":"keep  spaces"}`, `{"s":"keep  spaces"}`},
		{`{"e2":"… € ‰"}`, `{"e2":"… € ‰"}`},
		{`12345678901234567890123`, `12345678901234567890123`},
		{`not json`, `"not json"`},
		{`not <json>`, `"not \u003cjson\u003e"`},
		{``, `""`},
	}
	for _, c := range cases {
		var b strings.Builder
		writeJSONPayload(&b, []byte(c.in))
		if b.String() != c.want {
			t.Errorf("writeJSONPayload(%q) = %q, want %q", c.in, b.String(), c.want)
		}
	}
}

func TestWriteJSONString(t *testing.T) {
	cases := []struct{ in, want string }{
		{"orders.new", `"orders.new"`},
		{"", `""`},
		{`a"b\c`, `"a\"b\\c"`},
		{"<x>&", `"\u003cx\u003e\u0026"`},
		{"tab\there", `"tab\there"`},
		{"\xff", `"\ufffd"`},
		{"\u2028", `"\u2028"`},
	}
	for _, c := range cases {
		var b strings.Builder
		writeJSONString(&b, c.in)
		if b.String() != c.want {
			t.Errorf("writeJSONString(%q) = %q, want %q", c.in, b.String(), c.want)
		}
	}
}

func BenchmarkWriteJSONPayload(b *testing.B) {
	cases := []struct {
		name string
		data []byte
	}{
		{name: "small-json", data: []byte(`{"value": 123, "ok": true}`)},
		{name: "large-json", data: []byte(`{"blob":"` + strings.Repeat("x", performanceLargePayloadBytes) + `"}`)},
		{name: "raw-string", data: []byte(strings.Repeat("not-json", 128))},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			var out strings.Builder
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				out.Reset()
				writeJSONPayload(&out, tc.data)
			}
		})
	}
}

// TestHandler_EventName: event_type takes a message's SSE event name from
// its topic or a header, and falls back to "message" when that gives no
// usable name (#142).
func TestHandler_EventName(t *testing.T) {
	msg := func(subject, header string) streamMessage {
		m := streamMessage{Subject: subject}
		if header != "-" {
			m.Header = nats.Header{"Event-Type": []string{header}}
		}
		return m
	}
	topic := &Handler{TopicPrefix: "events.", EventType: eventTypeTopic}
	header := &Handler{TopicPrefix: "events.", EventType: eventTypeHeader, EventTypeHeader: "Event-Type"}
	for _, c := range []struct {
		name string
		h    *Handler
		msg  streamMessage
		want string
	}{
		{"default", &Handler{TopicPrefix: "events."}, msg("events.orders", "created"), "message"},
		{"message", &Handler{TopicPrefix: "events.", EventType: eventTypeMessage}, msg("events.orders", "created"), "message"},
		{"topic", topic, msg("events.orders.created", "x"), "orders.created"},
		{"topic without the prefix", topic, msg("other.orders", "x"), "other.orders"},
		{"topic that is reserved", topic, msg("events.error", "x"), "message"},
		{"header", header, msg("events.orders", "order:created"), "order:created"},
		{"header missing", header, msg("events.orders", "-"), "message"},
		{"header empty", header, msg("events.orders", ""), "message"},
		{"header with a line break", header, msg("events.orders", "a\nevent: forged"), "message"},
		{"header reserved", header, msg("events.orders", "connected"), "message"},
		{"header of another case", &Handler{EventType: eventTypeHeader, EventTypeHeader: "event-type"}, msg("events.orders", "created"), "message"},
	} {
		if got := c.h.eventName(c.msg); got != c.want {
			t.Errorf("%s: eventName = %q, want %q", c.name, got, c.want)
		}
	}
	frame := topic.formatMessageEvent(streamMessage{Subject: "events.orders", Data: []byte(`{}`)}, time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)).Frame
	if !strings.HasPrefix(frame, "event: orders\ndata: ") {
		t.Fatalf("frame = %q, want the orders event", frame)
	}
}
