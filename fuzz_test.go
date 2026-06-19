// fuzz_test.go — fuzz targets for the security-critical predicates.
//
// Coverage-based testing covers the corpus we thought of; fuzzing
// throws random inputs at the same predicates and surfaces parser
// bugs, panics, and accept-by-accident edge cases that example-based
// tests can't find. Each Fuzz target seeds the corpus with the
// existing positive/negative examples from the unit suite so the
// engine starts from known-interesting inputs.
//
// CI runs each target for a short bounded time on PR via
// `go test -run '^$' -fuzz Fuzz... -fuzztime 30s .` Local runs can use
// a longer fuzztime for deeper exploration.
package nuts

import (
	"strings"
	"testing"
)

// FuzzIsValidTopic ensures the topic-character-class predicate never
// panics on random input and that any accepted topic is composed of
// only the documented character set (ASCII letters, digits, dot, dash,
// underscore) without leading/trailing/consecutive dots and no
// wildcards or system prefixes.
func FuzzIsValidTopic(f *testing.F) {
	for _, s := range []string{
		"", "orders", "orders.created", "orders_new", "orders-new",
		"a.b.c", ".", "..", "a..b", ".a", "a.",
		"*", ">", "$SYS", "orders.>", "orders.*",
		"orders.created\n", "orders/created", "événements",
		"a" + "\x00" + "b",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		// Predicate must not panic regardless of input.
		got := isValidTopic(in)
		if !got {
			return
		}
		// If accepted, verify the documented contract (helpers.go:117):
		// non-empty, ≤ 256 bytes, no '$' prefix (system subject), no
		// leading/trailing/consecutive dots, and only allowed bytes
		// (ASCII letters, digits, dot, dash, underscore).
		if in == "" {
			t.Fatal("isValidTopic accepted empty string")
		}
		const maxTopicLen = 256
		if len(in) > maxTopicLen {
			t.Fatalf("accepted topic of length %d (max %d): %q", len(in), maxTopicLen, in)
		}
		if in[0] == '$' {
			t.Fatalf("accepted system-subject topic with $-prefix: %q", in)
		}
		if in[0] == '.' || in[len(in)-1] == '.' {
			t.Fatalf("accepted topic with leading/trailing dot: %q", in)
		}
		for i := 0; i < len(in); i++ {
			c := in[i]
			ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
				(c >= '0' && c <= '9') || c == '.' || c == '-' || c == '_'
			if !ok {
				t.Fatalf("accepted topic %q has disallowed byte 0x%02x at position %d", in, c, i)
			}
			if i > 0 && c == '.' && in[i-1] == '.' {
				t.Fatalf("accepted topic %q has consecutive dots at position %d", in, i)
			}
		}
	})
}

// FuzzIsValidTopicFilter exercises the JWT-claim filter validator.
// Accepts:
//   - the bare wildcards "*" and ">"
//   - any non-empty filter ≤ 256 bytes, no '$' prefix, no leading/
//     trailing/consecutive dots, where each dot-separated token is
//     either "*" or ">" (the latter only as the final token) or a
//     non-empty string of [A-Za-z0-9_-].
//
// The fuzz body asserts every accepted input meets this contract so a
// regression that opened the filter to whitespace, NULs, $-prefixes,
// or '>' in non-final position would surface.
func FuzzIsValidTopicFilter(f *testing.F) {
	for _, s := range []string{
		"", "*", ">", "orders.>", "orders.created", "orders.*",
		"a.>", "a.b.>", "a..>", ".>", ">.", ".", "..", "a..b",
		"a/b", "ä", "a\x00b", "ABC", "A1.B2.C3",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got := isValidTopicFilter(in)
		if !got {
			return
		}
		// Contract: non-empty, ≤ 256 bytes, no '$' prefix.
		if in == "" {
			t.Fatal("isValidTopicFilter accepted empty string")
		}
		const maxFilterLen = 256
		if len(in) > maxFilterLen {
			t.Fatalf("accepted filter of length %d (max %d): %q", len(in), maxFilterLen, in)
		}
		if in[0] == '$' {
			t.Fatalf("accepted system-subject filter with $-prefix: %q", in)
		}
		// Bare wildcards are accepted and short-circuit token parsing.
		if in == "*" || in == ">" {
			return
		}
		// No leading/trailing/consecutive dots.
		if in[0] == '.' || in[len(in)-1] == '.' {
			t.Fatalf("accepted filter with leading/trailing dot: %q", in)
		}
		for i := 1; i < len(in); i++ {
			if in[i] == '.' && in[i-1] == '.' {
				t.Fatalf("accepted filter with consecutive dots: %q", in)
			}
		}
		// Token-level contract.
		tokens := strings.Split(in, ".")
		for idx, tok := range tokens {
			if tok == "" {
				t.Fatalf("accepted filter with empty token at index %d: %q", idx, in)
			}
			if tok == ">" {
				if idx != len(tokens)-1 {
					t.Fatalf("accepted '>' token not in final position (idx=%d/%d): %q", idx, len(tokens)-1, in)
				}
				continue
			}
			if tok == "*" {
				continue
			}
			for j := 0; j < len(tok); j++ {
				c := tok[j]
				ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
					(c >= '0' && c <= '9') || c == '-' || c == '_'
				if !ok {
					t.Fatalf("accepted filter token %q has disallowed byte 0x%02x: %q", tok, c, in)
				}
			}
		}
	})
}

// FuzzIsValidCookieName exercises the cookie-name validator. RFC 6265
// allows: letters, digits, and !#$%&'*+-.^_`|~ . Anything else must
// be rejected; the predicate must never panic.
func FuzzIsValidCookieName(f *testing.F) {
	for _, s := range []string{
		"", "session", "session_id", "X-Auth", "a.b.c",
		"contains space", "with;semicolon", "tab\tinside",
		"unicode_ä", "0", "A", "a" + "\x00" + "b", "@@@",
	} {
		f.Add(s)
	}
	const allowedPunct = "!#$%&'*+-.^_`|~"
	f.Fuzz(func(t *testing.T, in string) {
		got := isValidCookieName(in)
		if !got {
			return
		}
		if in == "" {
			t.Fatal("isValidCookieName accepted empty string")
		}
		for i := 0; i < len(in); i++ {
			c := in[i]
			ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
				(c >= '0' && c <= '9')
			if !ok {
				// Punctuation check via the allowed set.
				for j := 0; j < len(allowedPunct); j++ {
					if c == allowedPunct[j] {
						ok = true
						break
					}
				}
			}
			if !ok {
				t.Fatalf("accepted cookie name %q has disallowed byte 0x%02x at position %d", in, c, i)
			}
		}
	})
}

// FuzzSubjectMatchesFilter throws random subject/filter pairs at the
// matcher and asserts the matcher does not panic. We do not re-derive
// the matching contract — that's a separate reasoning task — but
// catching panics covers the most dangerous regression class.
func FuzzSubjectMatchesFilter(f *testing.F) {
	for _, pair := range []struct{ subject, filter string }{
		{"", ""}, {"orders", "orders.>"}, {"orders.created", "orders.*"},
		{"orders.created", ">"}, {"a.b.c", "a.b.c"}, {"a", "a.*"},
		{"a.b", "*.b"}, {".", "."}, {"a..b", "a..b"},
		{"orders", ""}, {"", "orders"},
	} {
		f.Add(pair.subject, pair.filter)
	}
	f.Fuzz(func(t *testing.T, subject, filter string) {
		_ = subjectMatchesFilter(subject, filter)
	})
}

// FuzzSubscriberTopicMatches ensures the JWT-claim subscriber matcher
// (used by canSubscribe) doesn't panic on arbitrary inputs.
func FuzzSubscriberTopicMatches(f *testing.F) {
	for _, pair := range []struct{ topic, filter string }{
		{"", ""}, {"orders.created", "orders.>"}, {"orders", "orders.*"},
		{"orders.created", "*"}, {"orders.created", ">"},
		{"a.b.c.d", "a.b.>"}, {"orders.created.gold", "orders.created.>"},
	} {
		f.Add(pair.topic, pair.filter)
	}
	f.Fuzz(func(t *testing.T, topic, filter string) {
		_ = subscriberTopicMatches(topic, filter)
	})
}
