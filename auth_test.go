package nuts

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestJWTHMACHash_AllAlgs(t *testing.T) {
	cases := []struct {
		alg    string
		factor func() hash.Hash
		ok     bool
	}{
		{alg: "HS256", factor: sha256.New, ok: true},
		{alg: "HS384", factor: sha512.New384, ok: true},
		{alg: "HS512", factor: sha512.New, ok: true},
		{alg: "RS256", ok: false},
		{alg: "", ok: false},
	}
	for _, c := range cases {
		t.Run(c.alg, func(t *testing.T) {
			got, err := jwtHMACHash(c.alg)
			if c.ok {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				if got().Size() != c.factor().Size() {
					t.Fatalf("hash size = %d, want %d", got().Size(), c.factor().Size())
				}
				return
			}
			if err == nil {
				t.Fatal("expected error for unsupported algorithm")
			}
		})
	}
}

func TestJWTNumericDate_Cases(t *testing.T) {
	t.Run("nil returns absent", func(t *testing.T) {
		ts, ok, err := jwtNumericDate(nil)
		if err != nil || ok || !ts.IsZero() {
			t.Fatalf("got (%v, %v, %v)", ts, ok, err)
		}
	})
	t.Run("valid number", func(t *testing.T) {
		ts, ok, err := jwtNumericDate(json.Number("1700000000"))
		if err != nil || !ok || ts.Unix() != 1700000000 {
			t.Fatalf("got (%v, %v, %v)", ts, ok, err)
		}
	})
	t.Run("non-numeric value rejected", func(t *testing.T) {
		if _, _, err := jwtNumericDate("not-a-number"); err == nil {
			t.Fatal("expected error for non-numeric value")
		}
	})
	t.Run("oversize integer rejected", func(t *testing.T) {
		if _, _, err := jwtNumericDate(json.Number("99999999999999999999")); err == nil {
			t.Fatal("expected Int64 error for oversize value")
		}
	})
}

func TestValidateJWTTimeClaims_ErrorPaths(t *testing.T) {
	now := time.Unix(1700000000, 0)
	t.Run("invalid exp surfaces wrapped error", func(t *testing.T) {
		err := validateJWTTimeClaims(map[string]interface{}{"exp": "not-a-number"}, now)
		if err == nil || !strings.Contains(err.Error(), "exp") {
			t.Fatalf("err = %v, want wrapped exp error", err)
		}
	})
	t.Run("invalid nbf surfaces wrapped error", func(t *testing.T) {
		err := validateJWTTimeClaims(map[string]interface{}{"nbf": "not-a-number"}, now)
		if err == nil || !strings.Contains(err.Error(), "nbf") {
			t.Fatalf("err = %v, want wrapped nbf error", err)
		}
	})
	t.Run("nbf in future rejects", func(t *testing.T) {
		nbf := json.Number("1700003600")
		if err := validateJWTTimeClaims(map[string]interface{}{"nbf": nbf}, now); err == nil {
			t.Fatal("expected nbf-in-future error")
		}
	})
	t.Run("nil claims pass", func(t *testing.T) {
		if err := validateJWTTimeClaims(map[string]interface{}{}, now); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestParseSubscribeClaim_NonStringEntryRejected(t *testing.T) {
	if _, err := parseSubscribeClaim([]interface{}{"orders.>", 42}); err == nil {
		t.Fatal("expected error for non-string subscribe entry")
	}
}

func TestIsValidTopicFilter_Cases(t *testing.T) {
	cases := []struct {
		filter string
		want   bool
	}{
		{filter: "", want: false},
		{filter: strings.Repeat("a", 257), want: false},
		{filter: "$JS.api.>", want: false},
		{filter: "*", want: true},
		{filter: ">", want: true},
		{filter: "orders.>", want: true},
		{filter: "tenant.*.events", want: true},
		{filter: "..bad", want: false},
		{filter: ".bad", want: false},
		{filter: "bad.", want: false},
		{filter: "orders.>.created", want: false},
		{filter: "orders.bad/path", want: false},
		{filter: "orders.under_score-1", want: true},
	}
	for _, c := range cases {
		t.Run(c.filter, func(t *testing.T) {
			if got := isValidTopicFilter(c.filter); got != c.want {
				t.Fatalf("isValidTopicFilter(%q) = %v, want %v", c.filter, got, c.want)
			}
		})
	}
}

func TestExtractSubscriberToken_InvalidAuthHeaderShapes(t *testing.T) {
	h := &Handler{}
	cases := []string{
		"Token abc.def.ghi",
		"Bearer",
		"Bearer  ",
		"Basic dXNlcjpwYXNz",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/events?topic=x", nil)
			req.Header.Set("Authorization", raw)
			if _, err := h.extractSubscriberToken(req); err == nil {
				t.Fatalf("expected error for Authorization=%q", raw)
			}
		})
	}
}

func TestVerifySubscriberJWT_DecodeErrors(t *testing.T) {
	secret := []byte("test-secret")
	now := time.Unix(1700000000, 0)
	validPayload := encodeJWTPartTesting(t, map[string]interface{}{"subscribe": "*", "exp": now.Add(time.Hour).Unix()})

	t.Run("header b64 invalid", func(t *testing.T) {
		token := "!!!." + validPayload + "." + base64.RawURLEncoding.EncodeToString([]byte("sig"))
		if _, err := verifySubscriberJWT(token, secret, now); err == nil {
			t.Fatal("expected header decode error")
		}
	})
	t.Run("header JSON invalid", func(t *testing.T) {
		badHeader := base64.RawURLEncoding.EncodeToString([]byte("not-json"))
		token := badHeader + "." + validPayload + "." + base64.RawURLEncoding.EncodeToString([]byte("sig"))
		if _, err := verifySubscriberJWT(token, secret, now); err == nil {
			t.Fatal("expected header parse error")
		}
	})
	t.Run("signature b64 invalid", func(t *testing.T) {
		header := encodeJWTPartTesting(t, map[string]interface{}{"alg": "HS256", "typ": "JWT"})
		token := header + "." + validPayload + ".!!!"
		if _, err := verifySubscriberJWT(token, secret, now); err == nil {
			t.Fatal("expected signature decode error")
		}
	})
	t.Run("payload b64 invalid", func(t *testing.T) {
		header := encodeJWTPartTesting(t, map[string]interface{}{"alg": "HS256", "typ": "JWT"})
		badPayload := "!!!"
		mac := hmac.New(sha256.New, secret)
		mac.Write([]byte(header + "." + badPayload))
		token := header + "." + badPayload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
		if _, err := verifySubscriberJWT(token, secret, now); err == nil {
			t.Fatal("expected payload decode error")
		}
	})
	t.Run("payload JSON invalid", func(t *testing.T) {
		header := encodeJWTPartTesting(t, map[string]interface{}{"alg": "HS256", "typ": "JWT"})
		badPayload := base64.RawURLEncoding.EncodeToString([]byte("not-json"))
		mac := hmac.New(sha256.New, secret)
		mac.Write([]byte(header + "." + badPayload))
		token := header + "." + badPayload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
		if _, err := verifySubscriberJWT(token, secret, now); err == nil {
			t.Fatal("expected payload parse error")
		}
	})
	t.Run("HS512 token verifies", func(t *testing.T) {
		header := encodeJWTPartTesting(t, map[string]interface{}{"alg": "HS512", "typ": "JWT"})
		payload := encodeJWTPartTesting(t, map[string]interface{}{"subscribe": "*"})
		mac := hmac.New(sha512.New, secret)
		mac.Write([]byte(header + "." + payload))
		token := header + "." + payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
		if _, err := verifySubscriberJWT(token, secret, now); err != nil {
			t.Fatalf("HS512 token rejected: %v", err)
		}
	})
}

// TestAuth_LimitBoundaries exercises the three quantitative caps in
// auth.go right at their boundaries (limit-1, limit, limit+1). Without
// boundary tests a regression that flips `>` to `>=` or bumps a
// constant by one would not be caught by example-based coverage.
//
// - maxSubscriberJWTLen (8192): the compact token length cap.
// - maxSubscriberJWTDecodedSegmentLen (6144): a decoded segment cap.
// - maxSubscribeClaimFilters (128): how many "subscribe" entries a
//   token may carry.
func TestAuth_LimitBoundaries(t *testing.T) {
	now := time.Now()
	secret := []byte("test-secret-with-enough-entropy-1234567890")

	// Three-point boundary on auth.go:141 — `len(token) > maxSubscriberJWTLen`.
	// Builds a STRUCTURALLY VALID three-segment HS256 token whose total wire
	// length is exactly the requested size by padding the payload with an
	// unused `pad` claim. Without this padding-aware probe the previous
	// implementation tested ~120-byte tokens and dot-less strings, neither
	// of which exercises the actual length-cap branch — a regression
	// flipping `>` to `>=` would pass all those subtests.
	buildSizedToken := func(t *testing.T, totalLen int) string {
		t.Helper()
		header := encodeJWTPartTesting(t, map[string]interface{}{"alg": "HS256", "typ": "JWT"})
		// Iterative calibration: each loop adjusts pad to converge on the
		// target. Worst case we converge in a couple of iterations because
		// each padded byte adds a known amount of base64-encoded output.
		pad := 1
		for i := 0; i < 16; i++ {
			payloadObj := map[string]interface{}{
				"subscribe": "*",
				"pad":       strings.Repeat("A", pad),
			}
			payload := encodeJWTPartTesting(t, payloadObj)
			mac := hmac.New(sha256.New, secret)
			mac.Write([]byte(header + "." + payload))
			sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
			token := header + "." + payload + "." + sig
			diff := totalLen - len(token)
			if diff == 0 {
				return token
			}
			// Each character added to pad adds 1 to the encoded payload
			// (base64 with potential alignment). Add diff but never less
			// than 1 to make progress.
			pad += diff
			if pad < 1 {
				pad = 1
			}
		}
		t.Fatalf("buildSizedToken: did not converge to length %d (last pad=%d)", totalLen, pad)
		return ""
	}

	t.Run("token one below length limit accepted", func(t *testing.T) {
		token := buildSizedToken(t, maxSubscriberJWTLen-1)
		if len(token) != maxSubscriberJWTLen-1 {
			t.Fatalf("calibration failed: len=%d want=%d", len(token), maxSubscriberJWTLen-1)
		}
		if _, err := verifySubscriberJWT(token, secret, now); err != nil {
			t.Fatalf("token at maxSubscriberJWTLen-1 unexpectedly rejected: %v", err)
		}
	})

	t.Run("token exactly at length limit accepted", func(t *testing.T) {
		// auth.go:141 is `len(token) > maxSubscriberJWTLen` — equality must
		// be ACCEPTED. A regression to `>=` would fail this case with the
		// specific "token exceeds maximum length" error.
		token := buildSizedToken(t, maxSubscriberJWTLen)
		if len(token) != maxSubscriberJWTLen {
			t.Fatalf("calibration failed: len=%d want=%d", len(token), maxSubscriberJWTLen)
		}
		if _, err := verifySubscriberJWT(token, secret, now); err != nil {
			t.Fatalf("token at exact maxSubscriberJWTLen unexpectedly rejected: %v", err)
		}
	})

	t.Run("token one over length limit rejected with the length-cap error", func(t *testing.T) {
		token := buildSizedToken(t, maxSubscriberJWTLen+1)
		if len(token) != maxSubscriberJWTLen+1 {
			t.Fatalf("calibration failed: len=%d want=%d", len(token), maxSubscriberJWTLen+1)
		}
		// Assert the SPECIFIC error so a regression that lets the token
		// through the length cap but fails later (e.g. wrong segment count)
		// doesn't silently pass this case.
		_, err := verifySubscriberJWT(token, secret, now)
		if err == nil {
			t.Fatal("expected token > maxSubscriberJWTLen to be rejected")
		}
		if !strings.Contains(err.Error(), "exceeds maximum length") {
			t.Fatalf("expected length-cap error, got %v", err)
		}
	})

	t.Run("subscribe claim at filter count limit accepted", func(t *testing.T) {
		header := encodeJWTPartTesting(t, map[string]interface{}{"alg": "HS256", "typ": "JWT"})
		filters := make([]string, maxSubscribeClaimFilters)
		for i := range filters {
			filters[i] = fmt.Sprintf("topic.f%d", i)
		}
		payload := encodeJWTPartTesting(t, map[string]interface{}{"subscribe": filters})
		mac := hmac.New(sha256.New, secret)
		mac.Write([]byte(header + "." + payload))
		token := header + "." + payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
		if _, err := verifySubscriberJWT(token, secret, now); err != nil {
			t.Fatalf("subscribe with %d filters unexpectedly rejected: %v", maxSubscribeClaimFilters, err)
		}
	})

	t.Run("subscribe claim above filter count limit rejected", func(t *testing.T) {
		filters := make([]string, maxSubscribeClaimFilters+1)
		for i := range filters {
			filters[i] = fmt.Sprintf("topic.f%d", i)
		}
		_, err := parseSubscribeClaim(toInterfaceSlice(filters))
		if err == nil {
			t.Fatal("expected parseSubscribeClaim to reject > limit filters")
		}
		if !strings.Contains(err.Error(), "too many entries") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("decoded segment over limit rejected", func(t *testing.T) {
		oversized := strings.Repeat("A", maxSubscriberJWTDecodedSegmentLen*2)
		encoded := base64.RawURLEncoding.EncodeToString([]byte(oversized))
		_, err := decodeJWTSegment(encoded)
		if err == nil || !strings.Contains(err.Error(), "maximum decoded length") {
			t.Fatalf("expected decoded-length error, got %v", err)
		}
	})

	t.Run("decoded segment at limit accepted", func(t *testing.T) {
		justRight := strings.Repeat("A", maxSubscriberJWTDecodedSegmentLen)
		encoded := base64.RawURLEncoding.EncodeToString([]byte(justRight))
		if _, err := decodeJWTSegment(encoded); err != nil {
			t.Fatalf("expected decoded segment at limit to be accepted, got %v", err)
		}
	})
}

func toInterfaceSlice(s []string) []interface{} {
	out := make([]interface{}, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func encodeJWTPartTesting(t *testing.T, value interface{}) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JWT part: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// TestAuthorizeStreamRequest_RejectionMetrics asserts that each rejection
// branch in authorizeStreamRequest bumps the documented label on
// nuts_connections_rejected_total. Without this an attacker probing the
// auth surface leaves no Prometheus footprint (see ops/prometheus-alerts.yml's
// NutsAuthRejectionsHigh alert).
func TestAuthorizeStreamRequest_RejectionMetrics(t *testing.T) {
	secret := "test-secret-with-enough-entropy-1234567890"
	now := time.Now()
	h := &Handler{SubscriberJWTKey: secret}
	plan := streamPlan{Topics: []string{"orders.created"}}

	t.Run("auth_missing_token", func(t *testing.T) {
		before := counterValue(metricsConnectionsRejected, "auth_missing_token")
		req := httptest.NewRequest(http.MethodGet, "/events?topic=orders.created", nil)
		err := h.authorizeStreamRequest(req, plan)
		if err == nil || err.status != http.StatusUnauthorized {
			t.Fatalf("want 401 token-missing rejection, got %#v", err)
		}
		if got := counterValue(metricsConnectionsRejected, "auth_missing_token"); got <= before {
			t.Errorf("auth_missing_token counter did not increment: %v -> %v", before, got)
		}
	})

	t.Run("auth_invalid_token", func(t *testing.T) {
		before := counterValue(metricsConnectionsRejected, "auth_invalid_token")
		// Structurally valid HS256 token signed with a different key.
		header := encodeJWTPartTesting(t, map[string]interface{}{"alg": "HS256", "typ": "JWT"})
		payload := encodeJWTPartTesting(t, map[string]interface{}{"subscribe": "*", "exp": now.Add(time.Hour).Unix()})
		mac := hmac.New(sha256.New, []byte("wrong-secret"))
		mac.Write([]byte(header + "." + payload))
		token := header + "." + payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

		req := httptest.NewRequest(http.MethodGet, "/events?topic=orders.created", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		err := h.authorizeStreamRequest(req, plan)
		if err == nil || err.status != http.StatusUnauthorized {
			t.Fatalf("want 401 invalid-token rejection, got %#v", err)
		}
		if got := counterValue(metricsConnectionsRejected, "auth_invalid_token"); got <= before {
			t.Errorf("auth_invalid_token counter did not increment: %v -> %v", before, got)
		}
	})

	t.Run("auth_topic_forbidden", func(t *testing.T) {
		before := counterValue(metricsConnectionsRejected, "auth_topic_forbidden")
		// Valid token whose subscribe claim does NOT include orders.created.
		header := encodeJWTPartTesting(t, map[string]interface{}{"alg": "HS256", "typ": "JWT"})
		payload := encodeJWTPartTesting(t, map[string]interface{}{
			"subscribe": []string{"other.topic"},
			"exp":       now.Add(time.Hour).Unix(),
		})
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(header + "." + payload))
		token := header + "." + payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

		req := httptest.NewRequest(http.MethodGet, "/events?topic=orders.created", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		err := h.authorizeStreamRequest(req, plan)
		if err == nil || err.status != http.StatusForbidden {
			t.Fatalf("want 403 topic-forbidden rejection, got %#v", err)
		}
		if got := counterValue(metricsConnectionsRejected, "auth_topic_forbidden"); got <= before {
			t.Errorf("auth_topic_forbidden counter did not increment: %v -> %v", before, got)
		}
	})
}
