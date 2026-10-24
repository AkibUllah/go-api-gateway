// Package auth verifies JWT bearer tokens and API keys.
package auth

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/AkibUllah/go-api-gateway/internal/config"
)

// Principal is the authenticated caller.
type Principal struct {
	Subject string
	// Consumer is a stable identifier used for rate limiting and logs:
	// "jwt:<sub>" or "key:<name>".
	Consumer string
	Claims   jwt.MapClaims
}

// ErrUnauthorized wraps every authentication failure.
var ErrUnauthorized = errors.New("unauthorized")

// JWTVerifier validates tokens for one configured provider.
type JWTVerifier struct {
	cfg     config.JWTProvider
	parser  *jwt.Parser
	keyFunc jwt.Keyfunc
}

// NewJWTVerifier builds a verifier. jwks may be nil unless cfg.JWKSURL is set.
func NewJWTVerifier(cfg config.JWTProvider, jwks *JWKS) (*JWTVerifier, error) {
	opts := []jwt.ParserOption{
		jwt.WithValidMethods(cfg.Algorithms),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(cfg.Leeway.D()),
	}
	if cfg.Issuer != "" {
		opts = append(opts, jwt.WithIssuer(cfg.Issuer))
	}
	if cfg.Audience != "" {
		opts = append(opts, jwt.WithAudience(cfg.Audience))
	}
	v := &JWTVerifier{cfg: cfg, parser: jwt.NewParser(opts...)}

	switch {
	case cfg.Secret != "":
		secret := []byte(cfg.Secret)
		v.keyFunc = func(*jwt.Token) (any, error) { return secret, nil }
	case cfg.PublicKeyFile != "":
		pem, err := os.ReadFile(cfg.PublicKeyFile)
		if err != nil {
			return nil, fmt.Errorf("jwt public key: %w", err)
		}
		key, err := jwt.ParseRSAPublicKeyFromPEM(pem)
		if err != nil {
			return nil, fmt.Errorf("jwt public key: %w", err)
		}
		v.keyFunc = func(*jwt.Token) (any, error) { return key, nil }
	case cfg.JWKSURL != "":
		if jwks == nil {
			return nil, errors.New("jwt: jwks_url configured but no JWKS client given")
		}
		v.keyFunc = func(t *jwt.Token) (any, error) {
			kid, _ := t.Header["kid"].(string)
			if kid == "" {
				return nil, errors.New("token has no kid header")
			}
			key, err := jwks.Key(kid)
			if err != nil {
				return nil, err
			}
			return key, nil
		}
	default:
		return nil, errors.New("jwt: no key source configured")
	}
	return v, nil
}

// Verify parses and validates a raw token string.
func (v *JWTVerifier) Verify(raw string) (*Principal, error) {
	claims := jwt.MapClaims{}
	if _, err := v.parser.ParseWithClaims(raw, claims, v.keyFunc); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnauthorized, err)
	}
	sub, _ := claims.GetSubject()
	if sub == "" {
		return nil, fmt.Errorf("%w: token has no sub claim", ErrUnauthorized)
	}
	return &Principal{Subject: sub, Consumer: "jwt:" + sub, Claims: claims}, nil
}

// ForwardClaims maps claim names to the request headers they should be copied
// into for the upstream (e.g. sub -> X-User-ID).
func (v *JWTVerifier) ForwardClaims() map[string]string { return v.cfg.ForwardClaims }

// BearerToken extracts the token from an Authorization header value.
func BearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// ClaimString renders a claim value as a header-safe string. Arrays are
// comma-joined; nested objects are not forwarded.
func ClaimString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x)), true
		}
		return fmt.Sprintf("%g", x), true
	case bool:
		return fmt.Sprintf("%t", x), true
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			s, ok := ClaimString(e)
			if !ok {
				return "", false
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, ","), true
	}
	return "", false
}

// SignHS256 mints a token; used by the devtoken command and tests.
func SignHS256(secret string, claims jwt.MapClaims, ttl time.Duration) (string, error) {
	now := time.Now()
	if _, ok := claims["iat"]; !ok {
		claims["iat"] = now.Unix()
	}
	if _, ok := claims["exp"]; !ok {
		claims["exp"] = now.Add(ttl).Unix()
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}
