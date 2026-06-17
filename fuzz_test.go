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
		// If accepted, verify the contract: only allowed bytes; no
		// leading/trailing/consecutive dots; non-empty.
		if in == "" {
			t.Fatal("isValidTopic accepted empty string")
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
// Accepted filters must use only the documented character set
// (letters, digits, dash, underscore in each token) plus dots as
// segment separators; the bare `>` and `>` as the final token are
// also accepted as the NATS multi-wildcard. The predicate must never
// panic.
func FuzzIsValidTopicFilter(f *testing.F) {
	for _, s := range []string{
		"", "*", ">", "orders.>", "orders.created", "orders.*",
		"a.>", "a.b.>", "a..>", ".>", ">.", ".", "..", "a..b",
		"a/b", "ä", "a\x00b", "ABC", "A1.B2.C3",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		_ = isValidTopicFilter(in)
		// Acceptance contract is more nuanced than isValidTopic (wildcards
		// are allowed); we don't re-implement it here. The panic-freedom
		// guarantee alone is the major fuzz contract.
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
