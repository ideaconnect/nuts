// fuzz_test.go — fuzz targets for the security-critical predicates.
//
// Coverage-based testing covers the corpus we thought of; fuzzing
// throws random inputs at the same predicates and surfaces parser
// bugs, panics, and accept-by-accident edge cases that example-based
// tests can't find. Each Fuzz target seeds the corpus with the
// existing positive/negative examples from the unit suite so the
// engine starts from known-interesting inputs.
//
// Every `go test` run executes the seed corpus as table tests. The
// nightly Fuzz workflow (.github/workflows/fuzz.yml) runs each target
// under the fuzz engine for five minutes; locally, use for example
// `go test -run '^$' -fuzz '^FuzzIsValidTopic$' -fuzztime 1m .`
package nuts

import (
	"strconv"
	"strings"
	"testing"
)

// topicContractViolation says why in breaks isValidTopic's documented
// contract, or "" when it meets it: non-empty, at most 256 bytes, no '$'
// prefix (system subjects), no leading, trailing or consecutive dots, and
// only ASCII letters, digits, dot, dash and underscore. Written out apart
// from the validator, so the fuzzer can hold it to the contract both ways.
func topicContractViolation(in string) string {
	switch {
	case in == "":
		return "empty"
	case len(in) > maxSubjectLen:
		return "longer than 256 bytes"
	case in[0] == '$':
		return "system-subject $ prefix"
	case in[0] == '.' || in[len(in)-1] == '.':
		return "leading or trailing dot"
	}
	for i := 0; i < len(in); i++ {
		c := in[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '.' || c == '-' || c == '_'
		if !ok {
			return "disallowed byte 0x" + strconv.FormatUint(uint64(c), 16) + " at " + strconv.Itoa(i)
		}
		if i > 0 && c == '.' && in[i-1] == '.' {
			return "consecutive dots at " + strconv.Itoa(i)
		}
	}
	return ""
}

// FuzzIsValidTopic holds isValidTopic to its documented contract in both
// directions: it accepts no topic the contract forbids (wildcards, system
// prefixes, stray dots, bytes outside the set) and rejects none it allows.
func FuzzIsValidTopic(f *testing.F) {
	for _, s := range []string{
		"", "orders", "orders.created", "orders_new", "orders-new",
		"a.b.c", ".", "..", "a..b", ".a", "a.",
		"*", ">", "$SYS", "orders.>", "orders.*",
		"orders.created\n", "orders/created", "événements",
		"a" + "\x00" + "b",
		strings.Repeat("a", maxSubjectLen), strings.Repeat("a", maxSubjectLen+1),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		violation := topicContractViolation(in)
		switch got := isValidTopic(in); {
		case got && violation != "":
			t.Fatalf("isValidTopic accepted %q: %s", in, violation)
		case !got && violation == "":
			t.Fatalf("isValidTopic rejected %q, which meets the documented contract", in)
		}
	})
}

// topicFilterContractViolation says why in breaks isValidTopicFilter's
// documented contract, or "" when it meets it: the bare wildcards "*" and
// ">", or a non-empty filter of at most 256 bytes, without a '$' prefix or
// leading, trailing or consecutive dots, whose dot-separated tokens are "*",
// ">" (final token only) or non-empty runs of [A-Za-z0-9_-].
func topicFilterContractViolation(in string) string {
	switch {
	case in == "":
		return "empty"
	case len(in) > maxSubjectLen:
		return "longer than 256 bytes"
	case in[0] == '$':
		return "system-subject $ prefix"
	case in == "*" || in == ">":
		return ""
	case in[0] == '.' || in[len(in)-1] == '.':
		return "leading or trailing dot"
	}
	tokens := strings.Split(in, ".")
	for idx, tok := range tokens {
		switch tok {
		case "":
			return "empty token at " + strconv.Itoa(idx)
		case ">":
			if idx != len(tokens)-1 {
				return "'>' before the final token"
			}
			continue
		case "*":
			continue
		}
		for j := 0; j < len(tok); j++ {
			c := tok[j]
			ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
				(c >= '0' && c <= '9') || c == '-' || c == '_'
			if !ok {
				return "token " + strconv.Quote(tok) + " has disallowed byte 0x" + strconv.FormatUint(uint64(c), 16)
			}
		}
	}
	return ""
}

// FuzzIsValidTopicFilter holds the JWT-claim filter validator to its
// documented contract in both directions, so a regression that opened the
// filter to whitespace, NULs, $-prefixes or a non-final '>', or one that
// started refusing legitimate filters, surfaces.
func FuzzIsValidTopicFilter(f *testing.F) {
	for _, s := range []string{
		"", "*", ">", "orders.>", "orders.created", "orders.*",
		"a.>", "a.b.>", "a..>", ".>", ">.", ".", "..", "a..b",
		"a/b", "ä", "a\x00b", "ABC", "A1.B2.C3", "*.*.>", "a.>.b",
		"tenant-1.orders_new.*", "orders-created",
		strings.Repeat("a", maxSubjectLen), strings.Repeat("a", maxSubjectLen+1),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		violation := topicFilterContractViolation(in)
		switch got := isValidTopicFilter(in); {
		case got && violation != "":
			t.Fatalf("isValidTopicFilter accepted %q: %s", in, violation)
		case !got && violation == "":
			t.Fatalf("isValidTopicFilter rejected %q, which meets the documented contract", in)
		}
	})
}

// cookieNameContractViolation says why in is not an RFC 6265 cookie name,
// or "" when it is one: a non-empty run of letters, digits and
// !#$%&'*+-.^_`|~ .
func cookieNameContractViolation(in string) string {
	const allowedPunct = "!#$%&'*+-.^_`|~"
	if in == "" {
		return "empty"
	}
	for i := 0; i < len(in); i++ {
		c := in[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || strings.IndexByte(allowedPunct, c) >= 0
		if !ok {
			return "disallowed byte 0x" + strconv.FormatUint(uint64(c), 16) + " at " + strconv.Itoa(i)
		}
	}
	return ""
}

// FuzzIsValidCookieName holds the cookie-name validator to RFC 6265 in both
// directions.
func FuzzIsValidCookieName(f *testing.F) {
	for _, s := range []string{
		"", "session", "session_id", "X-Auth", "a.b.c",
		"contains space", "with;semicolon", "tab\tinside",
		"unicode_ä", "0", "A", "a" + "\x00" + "b", "@@@", "!#$%&'*+-.^_`|~",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		violation := cookieNameContractViolation(in)
		switch got := isValidCookieName(in); {
		case got && violation != "":
			t.Fatalf("isValidCookieName accepted %q: %s", in, violation)
		case !got && violation == "":
			t.Fatalf("isValidCookieName rejected %q, which meets RFC 6265", in)
		}
	})
}

// referenceSubjectMatchesFilter is the matcher as it was before it stopped
// allocating (#124), kept to pin the rewrite to the same answers.
func referenceSubjectMatchesFilter(subject, filter string) bool {
	if subject == "" || filter == "" {
		return false
	}
	subjectTokens := strings.Split(subject, ".")
	filterTokens := strings.Split(filter, ".")
	for idx, filterToken := range filterTokens {
		if filterToken == ">" {
			return idx < len(subjectTokens)
		}
		if idx >= len(subjectTokens) {
			return false
		}
		if filterToken != "*" && filterToken != subjectTokens[idx] {
			return false
		}
	}
	return len(subjectTokens) == len(filterTokens)
}

// FuzzSubjectMatchesFilter checks the matcher against its reference and
// against properties of NATS subject matching (#61): an empty subject or
// filter never matches, ">" matches any subject, a subject matches itself,
// and "*" stands for exactly one token.
func FuzzSubjectMatchesFilter(f *testing.F) {
	for _, pair := range []struct{ subject, filter string }{
		{"", ""}, {"orders", "orders.>"}, {"orders.created", "orders.*"},
		{"orders.created", ">"}, {"a.b.c", "a.b.c"}, {"a", "a.*"},
		{"a.b", "*.b"}, {".", "."}, {"a..b", "a..b"}, {"a..b", "a.*.b"},
		{"orders", ""}, {"", "orders"}, {"a.b", "a.>.c"}, {"a.", "a.*"},
		{"a.b.c", "*.*"}, {"a", "*.*"}, {"a.b", "a.b.c"},
	} {
		f.Add(pair.subject, pair.filter)
	}
	f.Fuzz(func(t *testing.T, subject, filter string) {
		got := subjectMatchesFilter(subject, filter)
		if want := referenceSubjectMatchesFilter(subject, filter); got != want {
			t.Fatalf("subjectMatchesFilter(%q, %q) = %v, reference says %v", subject, filter, got, want)
		}
		if (subject == "" || filter == "") && got {
			t.Fatalf("subjectMatchesFilter(%q, %q) matched an empty subject or filter", subject, filter)
		}
		if subject != "" && !subjectMatchesFilter(subject, ">") {
			t.Fatalf("subjectMatchesFilter(%q, \">\") = false", subject)
		}
		if subject != "" && !subjectMatchesFilter(subject, subject) {
			t.Fatalf("subjectMatchesFilter(%q, itself) = false", subject)
		}
		tokens := strings.Split(subject, ".")
		stars := strings.TrimSuffix(strings.Repeat("*.", len(tokens)), ".")
		if subject != "" && !subjectMatchesFilter(subject, stars) {
			t.Fatalf("subjectMatchesFilter(%q, %q) = false, one * per token", subject, stars)
		}
		if subject != "" && subjectMatchesFilter(subject, stars+".*") {
			t.Fatalf("subjectMatchesFilter(%q, %q) = true with one * too many", subject, stars+".*")
		}
	})
}

// FuzzSubscriberTopicMatches checks the JWT-claim matcher, which gates
// topic authorization: a bare "*" or ">" allows any topic, and every other
// filter defers to subjectMatchesFilter.
func FuzzSubscriberTopicMatches(f *testing.F) {
	for _, pair := range []struct{ topic, filter string }{
		{"", ""}, {"orders.created", "orders.>"}, {"orders", "orders.*"},
		{"orders.created", "*"}, {"orders.created", ">"},
		{"a.b.c.d", "a.b.>"}, {"orders.created.gold", "orders.created.>"},
	} {
		f.Add(pair.topic, pair.filter)
	}
	f.Fuzz(func(t *testing.T, topic, filter string) {
		got := subscriberTopicMatches(topic, filter)
		switch {
		case filter == "*" || filter == ">":
			if !got {
				t.Fatalf("subscriberTopicMatches(%q, %q) = false; a bare wildcard allows any topic", topic, filter)
			}
		case got != subjectMatchesFilter(topic, filter):
			t.Fatalf("subscriberTopicMatches(%q, %q) = %v, subjectMatchesFilter disagrees", topic, filter, got)
		}
	})
}

// BenchmarkCanSubscribeAtTheLimits authorizes 32 topics against a claim of
// 128 filters that match none of them: the worst case a token holder can
// ask for per request (#124).
func BenchmarkCanSubscribeAtTheLimits(b *testing.B) {
	claims := subscriberClaims{}
	for i := 0; i < maxSubscribeClaimFilters; i++ {
		claims.Subscribe = append(claims.Subscribe, "tenant-"+strconv.Itoa(i)+".orders.*")
	}
	topics := make([]string, defaultMaxTopicsPerSubscription)
	for i := range topics {
		topics[i] = "other-" + strconv.Itoa(i) + ".orders.created"
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for _, topic := range topics {
			if claims.canSubscribe(topic) {
				b.Fatal("unexpected match")
			}
		}
	}
}
