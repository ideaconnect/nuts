// auth.go — first-party subscriber authorization.
//
// NUTS is a public-by-default SSE bridge: when SubscriberJWTKey is empty,
// every request that passes topic validation is allowed. When the key is
// set, this file enforces an HMAC-signed JWT on each subscription:
//
//  1. Token is read from the Authorization: Bearer header, or from the
//     configured cookie when no header is present.
//  2. Signature is verified against SubscriberJWTKey using the algorithm
//     declared in the JWT header (HS256 / HS384 / HS512 only — RSA/ECDSA
//     are intentionally not supported, see jwtHMACHash).
//  3. Standard exp/nbf time claims are enforced.
//  4. The custom "subscribe" claim is parsed into a list of NATS-style
//     subject filters; the request is allowed only if every requested
//     topic is covered by at least one filter.
//
// The verifier deliberately does not depend on a third-party JWT library:
// supporting only HMAC keeps the trust surface tiny and avoids the alg=none
// and key-confusion classes of bug entirely.
package nuts

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

const (
	maxSubscriberJWTLen               = 8192
	maxSubscriberJWTDecodedSegmentLen = 6144
	maxSubscribeClaimFilters          = 128
)

// subscriberClaims is the verified, decoded form of a subscriber JWT — only
// the fields NUTS actually uses. Subject is purely informational (logged on
// authorization failures); Subscribe drives the per-topic decision in
// canSubscribe.
type subscriberClaims struct {
	Subject   string
	Subscribe []string
}

// authorizeStreamRequest enforces subscriber JWT auth before any JetStream
// subscription is opened. Returns nil to allow the request, or a
// streamRequestError carrying the HTTP status (401 for credential
// problems, 403 for an authenticated subscriber asking for a topic their
// claim doesn't cover) and a sanitised public message — verification
// errors are logged in detail but never returned to the client, so a
// probing attacker cannot tell apart "bad signature" from "expired" from
// "missing claim".
//
// When SubscriberJWTKey is unset this is a no-op: NUTS is public-by-
// default and the operator opts in to JWT auth by configuring the key.
func (h *Handler) authorizeStreamRequest(r *http.Request, plan streamPlan) *streamRequestError {
	if h.SubscriberJWTKey == "" {
		return nil
	}

	token, err := h.extractSubscriberToken(r)
	if err != nil {
		return &streamRequestError{status: http.StatusUnauthorized, message: err.Error()}
	}

	claims, err := verifySubscriberJWT(token, []byte(h.SubscriberJWTKey), time.Now())
	if err != nil {
		if h.logger != nil {
			h.logger.Warn("subscriber JWT rejected", appendStreamLogFields(plan, zap.Error(err))...)
		}
		return &streamRequestError{status: http.StatusUnauthorized, message: "Invalid subscriber token"}
	}

	for _, topic := range plan.Topics {
		if !claims.canSubscribe(topic) {
			if h.logger != nil {
				h.logger.Warn("subscriber is not authorized for requested topic",
					appendStreamLogFields(plan,
						zap.String("subscriber", claims.Subject),
						zap.String("unauthorized_topic", topic),
					)...,
				)
			}
			return &streamRequestError{status: http.StatusForbidden, message: "Forbidden topic"}
		}
	}

	return nil
}

// extractSubscriberToken pulls the JWT from the request. Header takes
// precedence over cookie: when both are present we honour what the client
// most recently and explicitly attached, and a malformed header is a hard
// error (rather than silently falling back to the cookie, which would
// mask client bugs and could enable downgrade-style attacks).
//
// The cookie path only runs when SubscriberJWTCookie is configured —
// browser EventSource cannot set custom headers, so cookies are the
// realistic transport for browser clients; for non-browser clients
// (curl, server-to-server) the Bearer header is the conventional choice.
func (h *Handler) extractSubscriberToken(r *http.Request) (string, error) {
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); auth != "" {
		parts := strings.Fields(auth)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") && parts[1] != "" {
			return parts[1], nil
		}
		return "", errors.New("Invalid Authorization header")
	}

	if h.SubscriberJWTCookie != "" {
		cookie, err := r.Cookie(h.SubscriberJWTCookie)
		if err == nil && cookie.Value != "" {
			return cookie.Value, nil
		}
	}

	return "", errors.New("Subscriber token required")
}

// verifySubscriberJWT parses and verifies a compact JWT against the
// configured HMAC key. Steps:
//
//  1. Split into three base64url segments; reject anything else.
//  2. Decode the header to read the "alg" field, then map it to a hash
//     constructor via jwtHMACHash (HS256/384/512 only — see file header
//     for why RSA/ECDSA is excluded).
//  3. Recompute the HMAC over `<header>.<payload>` and compare against the
//     decoded signature using hmac.Equal (constant-time).
//  4. Decode the payload with json.Number so numeric date claims survive
//     round-tripping without float precision loss.
//  5. Enforce exp/nbf, then parse the custom "subscribe" claim.
//
// `now` is injected so tests can verify exp/nbf boundary behavior without
// sleeping. Production callers pass time.Now().
func verifySubscriberJWT(token string, key []byte, now time.Time) (subscriberClaims, error) {
	if len(token) > maxSubscriberJWTLen {
		return subscriberClaims{}, errors.New("token exceeds maximum length")
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return subscriberClaims{}, errors.New("token must have three segments")
	}

	headerBytes, err := decodeJWTSegment(parts[0])
	if err != nil {
		return subscriberClaims{}, fmt.Errorf("decode JWT header: %w", err)
	}
	var header struct {
		Algorithm string `json:"alg"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return subscriberClaims{}, fmt.Errorf("parse JWT header: %w", err)
	}

	newHash, err := jwtHMACHash(header.Algorithm)
	if err != nil {
		return subscriberClaims{}, err
	}
	signature, err := decodeJWTSegment(parts[2])
	if err != nil {
		return subscriberClaims{}, fmt.Errorf("decode JWT signature: %w", err)
	}
	mac := hmac.New(newHash, key)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	// hmac.Equal is constant-time; do not replace with bytes.Equal — the
	// timing difference would leak signature prefixes to a crafted attacker.
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return subscriberClaims{}, errors.New("JWT signature mismatch")
	}

	payloadBytes, err := decodeJWTSegment(parts[1])
	if err != nil {
		return subscriberClaims{}, fmt.Errorf("decode JWT payload: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payloadBytes))
	// UseNumber preserves exact integer precision for exp/nbf — large
	// timestamps decoded as float64 can round and silently shift the
	// expiry check by a second or more.
	decoder.UseNumber()
	claimsMap := map[string]interface{}{}
	if err := decoder.Decode(&claimsMap); err != nil {
		return subscriberClaims{}, fmt.Errorf("parse JWT payload: %w", err)
	}
	if err := validateJWTTimeClaims(claimsMap, now); err != nil {
		return subscriberClaims{}, err
	}

	subscribe, err := parseSubscribeClaim(claimsMap["subscribe"])
	if err != nil {
		return subscriberClaims{}, err
	}
	claims := subscriberClaims{Subscribe: subscribe}
	if subject, ok := claimsMap["sub"].(string); ok {
		claims.Subject = subject
	}
	return claims, nil
}

// decodeJWTSegment decodes one base64url segment of a JWT. JWT uses the
// URL-safe alphabet without padding (RawURLEncoding), not standard base64.
func decodeJWTSegment(segment string) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return nil, err
	}
	if len(decoded) > maxSubscriberJWTDecodedSegmentLen {
		return nil, errors.New("JWT segment exceeds maximum decoded length")
	}
	return decoded, nil
}

// jwtHMACHash returns the hash constructor for a supported JWT algorithm.
// Only HMAC variants are accepted — asymmetric algorithms (RS*, ES*, PS*)
// are rejected outright. This is a deliberate scope limit: it eliminates
// the alg=none and HS256-with-RSA-public-key key-confusion classes of
// attack at the parser level rather than relying on careful key-type
// checks elsewhere.
func jwtHMACHash(alg string) (func() hash.Hash, error) {
	switch alg {
	case "HS256":
		return sha256.New, nil
	case "HS384":
		return sha512.New384, nil
	case "HS512":
		return sha512.New, nil
	default:
		return nil, fmt.Errorf("unsupported JWT algorithm %q", alg)
	}
}

// validateJWTTimeClaims enforces RFC 7519 exp and nbf claims when present.
// Both are optional; absence is not an error (the caller can require them
// at a higher layer if needed). Missing/zero values are treated as "claim
// not provided" rather than "claim is zero".
func validateJWTTimeClaims(claims map[string]interface{}, now time.Time) error {
	if exp, ok, err := jwtNumericDate(claims["exp"]); err != nil {
		return fmt.Errorf("invalid exp claim: %w", err)
	} else if ok && !now.Before(exp) {
		return errors.New("JWT is expired")
	}

	if nbf, ok, err := jwtNumericDate(claims["nbf"]); err != nil {
		return fmt.Errorf("invalid nbf claim: %w", err)
	} else if ok && now.Before(nbf) {
		return errors.New("JWT is not valid yet")
	}

	return nil
}

// jwtNumericDate converts a NumericDate claim value (epoch seconds) to a
// time.Time. Returns (zero, false, nil) when the claim is absent so the
// caller can distinguish "missing" from "present but invalid". Requires
// json.Number; a string or boolean disguised as a date is an error rather
// than silently treated as missing.
func jwtNumericDate(value interface{}) (time.Time, bool, error) {
	if value == nil {
		return time.Time{}, false, nil
	}
	number, ok := value.(json.Number)
	if !ok {
		return time.Time{}, false, errors.New("must be a numeric date")
	}
	seconds, err := number.Int64()
	if err != nil {
		return time.Time{}, false, err
	}
	return time.Unix(seconds, 0), true, nil
}

// parseSubscribeClaim extracts and validates the "subscribe" claim, which
// carries the NATS-style subject filters this token is allowed to read.
// Accepts either a single string or an array of strings (issuers commonly
// emit a single-string form for the trivial case). The claim is required
// — a token without it is rejected — and every entry must pass
// isValidTopicFilter so authorization decisions are made on a known
// alphabet.
func parseSubscribeClaim(value interface{}) ([]string, error) {
	var filters []string
	switch typed := value.(type) {
	case string:
		filters = []string{typed}
	case []interface{}:
		for _, item := range typed {
			filter, ok := item.(string)
			if !ok {
				return nil, errors.New("subscribe entries must be strings")
			}
			filters = append(filters, filter)
		}
	default:
		return nil, errors.New("subscribe claim is required")
	}
	if len(filters) == 0 {
		return nil, errors.New("subscribe claim must not be empty")
	}
	if len(filters) > maxSubscribeClaimFilters {
		return nil, fmt.Errorf("subscribe claim has too many entries; maximum is %d", maxSubscribeClaimFilters)
	}

	for _, filter := range filters {
		if !isValidTopicFilter(filter) {
			return nil, fmt.Errorf("invalid subscribe filter %q", filter)
		}
	}
	return filters, nil
}

// canSubscribe reports whether any of the token's subscribe filters
// covers the requested topic. Authorization is permissive within the
// claim — one matching filter is enough.
func (c subscriberClaims) canSubscribe(topic string) bool {
	for _, filter := range c.Subscribe {
		if subscriberTopicMatches(topic, filter) {
			return true
		}
	}
	return false
}

// subscriberTopicMatches is the per-filter match used by canSubscribe.
// Bare "*" and ">" are both treated as "any topic" — NATS itself only
// gives ">" that meaning, but for subscriber claims the user-friendly
// "*" alias is also accepted. All other filters defer to NATS subject
// matching (see subjectMatchesFilter in serve.go).
func subscriberTopicMatches(topic, filter string) bool {
	if filter == "*" || filter == ">" {
		return true
	}
	return subjectMatchesFilter(topic, filter)
}

// isValidTopicFilter reports whether a string is a syntactically valid
// subject filter for use in the subscribe claim. The character set
// matches isValidTopic ([A-Za-z0-9._-]) plus the NATS wildcards "*" and
// ">" as whole tokens, with these guard rails:
//
//   - Empty, oversized (>256 chars), and "$"-prefixed filters are rejected
//     ("$"-prefixed subjects are NATS system subjects).
//   - "*" and ">" alone match any topic.
//   - ">" is only legal as the final token (matches the NATS rule).
//   - Empty tokens, leading/trailing/double dots are rejected.
//
// The intent is to fail fast on malformed claims before they reach the
// matcher, so an attacker can't smuggle weird whitespace or null bytes
// past authorization via a crafted JWT.
func isValidTopicFilter(filter string) bool {
	const maxFilterLen = 256
	if filter == "" || len(filter) > maxFilterLen || strings.HasPrefix(filter, "$") {
		return false
	}
	if filter == "*" || filter == ">" {
		return true
	}
	if strings.Contains(filter, "..") || strings.HasPrefix(filter, ".") || strings.HasSuffix(filter, ".") {
		return false
	}

	tokens := strings.Split(filter, ".")
	for idx, token := range tokens {
		if token == ">" {
			return idx == len(tokens)-1
		}
		if token == "*" {
			continue
		}
		if token == "" {
			return false
		}
		for i := 0; i < len(token); i++ {
			c := token[i]
			switch {
			case c >= 'a' && c <= 'z':
			case c >= 'A' && c <= 'Z':
			case c >= '0' && c <= '9':
			case c == '-' || c == '_':
			default:
				return false
			}
		}
	}
	return true
}
