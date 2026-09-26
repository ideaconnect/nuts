package nuts

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// referenceMessageFrame renders a message frame the way formatMessageEvent
// did before it was rewritten as a single pass (#122): compact the payload,
// wrap it in messageEventPayload and json.Marshal the envelope. The rewrite
// must stay byte-identical to it, since clients may depend on the exact
// escaping.
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
	if msg.HasMetadata {
		event.WriteString("id: ")
		event.WriteString(strconv.FormatUint(msg.StreamSequence, 10))
		event.WriteString("\n")
	}
	event.WriteString("event: message\n")
	event.WriteString("data: ")
	event.WriteString(toJSON(payload))
	event.WriteString("\n\n")
	return event.String()
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
	"{\"bad\":\"\xff\xfe\"}",
	"\xff\xfe not UTF-8",
	`{"a":1}{"b":2}`,
	`{"trailing":1,}`,
	strings.Repeat(`{"n":[`, 50) + strings.Repeat(`]}`, 50),
}

func TestFormatMessageEvent_MatchesTheReferenceFormatter(t *testing.T) {
	h := &Handler{TopicPrefix: "events.", MaxEventSize: -1}
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	for i, data := range formatterEdgeCases {
		for _, hasMeta := range []bool{true, false} {
			msg := streamMessage{Subject: "events.orders", Data: []byte(data), HasMetadata: hasMeta, StreamSequence: 42, Timestamp: now.Add(-time.Hour)}
			want := referenceMessageFrame(h, msg, now)
			if got := h.formatMessageEvent(msg, now).Frame; got != want {
				t.Errorf("case %d (%q, metadata=%v):\n got %q\nwant %q", i, data, hasMeta, got, want)
			}
		}
	}
}

func FuzzFormatMessageEventMatchesReference(f *testing.F) {
	for _, data := range formatterEdgeCases {
		f.Add([]byte(data), "orders", uint64(42), true)
	}
	h := &Handler{TopicPrefix: "events.", MaxEventSize: -1}
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, data []byte, topic string, seq uint64, hasMeta bool) {
		msg := streamMessage{Subject: "events." + topic, Data: data, HasMetadata: hasMeta, StreamSequence: seq, Timestamp: now.Add(-time.Minute)}
		want := referenceMessageFrame(h, msg, now)
		if got := h.formatMessageEvent(msg, now).Frame; got != want {
			t.Fatalf("frame differs for data=%q topic=%q:\n got %q\nwant %q", data, topic, got, want)
		}
	})
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
