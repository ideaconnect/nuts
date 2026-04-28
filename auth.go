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

type subscriberClaims struct {
	Subject   string
	Subscribe []string
}

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

func verifySubscriberJWT(token string, key []byte, now time.Time) (subscriberClaims, error) {
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
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return subscriberClaims{}, errors.New("JWT signature mismatch")
	}

	payloadBytes, err := decodeJWTSegment(parts[1])
	if err != nil {
		return subscriberClaims{}, fmt.Errorf("decode JWT payload: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payloadBytes))
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

func decodeJWTSegment(segment string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(segment)
}

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

	for _, filter := range filters {
		if !isValidTopicFilter(filter) {
			return nil, fmt.Errorf("invalid subscribe filter %q", filter)
		}
	}
	return filters, nil
}

func (c subscriberClaims) canSubscribe(topic string) bool {
	for _, filter := range c.Subscribe {
		if subscriberTopicMatches(topic, filter) {
			return true
		}
	}
	return false
}

func subscriberTopicMatches(topic, filter string) bool {
	if filter == "*" || filter == ">" {
		return true
	}
	return subjectMatchesFilter(topic, filter)
}

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
