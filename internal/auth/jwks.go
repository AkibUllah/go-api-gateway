package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// JWKS fetches and caches RSA signing keys from a JSON Web Key Set endpoint.
//
// Keys are cached for TTL. An unknown kid triggers an immediate refresh (key
// rotation), but at most once per MinRefresh so a flood of tokens with random
// kids cannot be used to hammer the identity provider.
type JWKS struct {
	URL        string
	TTL        time.Duration
	MinRefresh time.Duration
	Client     *http.Client
	Now        func() time.Time

	mu          sync.Mutex
	keys        map[string]*rsa.PublicKey
	fetchedAt   time.Time
	lastAttempt time.Time
}

// NewJWKS returns a JWKS cache with sane defaults.
func NewJWKS(url string, ttl time.Duration) *JWKS {
	return &JWKS{
		URL:        url,
		TTL:        ttl,
		MinRefresh: 10 * time.Second,
		Client:     &http.Client{Timeout: 5 * time.Second},
		Now:        time.Now,
	}
}

// ErrUnknownKID is returned when the kid is absent even after a refresh.
var ErrUnknownKID = errors.New("signing key not found in JWKS")

// Key returns the public key for kid, fetching the key set when needed.
func (j *JWKS) Key(kid string) (*rsa.PublicKey, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := j.Now()

	stale := j.keys == nil || now.Sub(j.fetchedAt) > j.TTL
	if key, ok := j.keys[kid]; ok && !stale {
		return key, nil
	}
	// Fetches are throttled even when the cache is stale, so an IdP outage
	// does not turn every request into a blocking fetch. Holding the lock
	// while fetching also collapses concurrent misses into one request.
	if j.lastAttempt.IsZero() || now.Sub(j.lastAttempt) >= j.MinRefresh {
		j.lastAttempt = now
		keys, err := j.fetch()
		if err != nil {
			// Keep serving the previous key set if the IdP is briefly down.
			if key, ok := j.keys[kid]; ok {
				return key, nil
			}
			return nil, err
		}
		j.keys, j.fetchedAt = keys, now
	}
	if key, ok := j.keys[kid]; ok {
		return key, nil
	}
	return nil, fmt.Errorf("%w: kid %q", ErrUnknownKID, kid)
}

type jwkSet struct {
	Keys []struct {
		Kty string `json:"kty"`
		Kid string `json:"kid"`
		Use string `json:"use"`
		Alg string `json:"alg"`
		N   string `json:"n"`
		E   string `json:"e"`
	} `json:"keys"`
}

func (j *JWKS) fetch() (map[string]*rsa.PublicKey, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := j.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch jwks: status %d", resp.StatusCode)
	}
	var set jwkSet
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("decode jwks: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		pub, err := rsaFromJWK(k.N, k.E)
		if err != nil {
			return nil, fmt.Errorf("jwks key %q: %w", k.Kid, err)
		}
		keys[k.Kid] = pub
	}
	return keys, nil
}

func rsaFromJWK(n64, e64 string) (*rsa.PublicKey, error) {
	nb, err := base64.RawURLEncoding.DecodeString(n64)
	if err != nil {
		return nil, fmt.Errorf("modulus: %w", err)
	}
	eb, err := base64.RawURLEncoding.DecodeString(e64)
	if err != nil {
		return nil, fmt.Errorf("exponent: %w", err)
	}
	e := new(big.Int).SetBytes(eb)
	if !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 {
		return nil, errors.New("exponent out of range")
	}
	n := new(big.Int).SetBytes(nb)
	if n.BitLen() < 2048 {
		return nil, errors.New("modulus shorter than 2048 bits")
	}
	return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
}

// EncodeJWK renders a public key as a JWK (used by tests and local demos).
func EncodeJWK(kid string, pub *rsa.PublicKey) map[string]string {
	return map[string]string{
		"kty": "RSA",
		"kid": kid,
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}
