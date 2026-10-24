package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/AkibUllah/go-api-gateway/internal/config"
)

const secret = "test-secret-0123456789abcdef-0123456789"

var (
	keyOnce sync.Once
	rsaKeys [2]*rsa.PrivateKey
)

func testKeys(t testing.TB) [2]*rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		for i := range rsaKeys {
			k, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				panic(err)
			}
			rsaKeys[i] = k
		}
	})
	return rsaKeys
}

func hsVerifier(t testing.TB) *JWTVerifier {
	t.Helper()
	v, err := NewJWTVerifier(config.JWTProvider{
		Algorithms: []string{"HS256"}, Secret: secret, Issuer: "https://auth.test", Audience: "api",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func claims(extra jwt.MapClaims) jwt.MapClaims {
	c := jwt.MapClaims{
		"sub": "customer-42", "iss": "https://auth.test", "aud": "api",
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	for k, v := range extra {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

func signHS(t testing.TB, c jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func signRS(t testing.TB, key *rsa.PrivateKey, kid string, c jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestHS256AcceptsValidToken(t *testing.T) {
	p, err := hsVerifier(t).Verify(signHS(t, claims(jwt.MapClaims{"role": "customer"})))
	if err != nil {
		t.Fatal(err)
	}
	if p.Subject != "customer-42" || p.Consumer != "jwt:customer-42" || p.Claims["role"] != "customer" {
		t.Fatalf("principal = %+v", p)
	}
}

func TestHS256RejectsBadTokens(t *testing.T) {
	v := hsVerifier(t)
	cases := map[string]string{
		"expired":                  signHS(t, claims(jwt.MapClaims{"exp": time.Now().Add(-time.Minute).Unix()})),
		"no exp":                   signHS(t, claims(jwt.MapClaims{"exp": nil})),
		"wrong issuer":             signHS(t, claims(jwt.MapClaims{"iss": "https://evil.test"})),
		"wrong audience":           signHS(t, claims(jwt.MapClaims{"aud": "other"})),
		"no subject":               signHS(t, claims(jwt.MapClaims{"sub": nil})),
		"garbage":                  "not.a.jwt",
		"tampered":                 signHS(t, claims(nil)) + "x",
		"rs256 for hs256 provider": signRS(t, testKeys(t)[0], "", claims(nil)),
	}
	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, claims(nil)).SignedString(jwt.UnsafeAllowNoneSignatureType)
	cases["alg none"] = none
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := v.Verify(tok); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("err = %v, want ErrUnauthorized", err)
			}
		})
	}
}

func TestLeewayToleratesClockSkew(t *testing.T) {
	v, _ := NewJWTVerifier(config.JWTProvider{
		Algorithms: []string{"HS256"}, Secret: secret, Leeway: config.Duration(time.Minute),
	}, nil)
	tok := signHS(t, claims(jwt.MapClaims{"exp": time.Now().Add(-30 * time.Second).Unix()}))
	if _, err := v.Verify(tok); err != nil {
		t.Fatalf("token 30s past exp rejected despite 1m leeway: %v", err)
	}
}

func TestRS256WithPublicKeyFile(t *testing.T) {
	key := testKeys(t)[0]
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	path := filepath.Join(t.TempDir(), "pub.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := NewJWTVerifier(config.JWTProvider{Algorithms: []string{"RS256"}, PublicKeyFile: path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(signRS(t, key, "", claims(nil))); err != nil {
		t.Fatalf("valid RS256 token rejected: %v", err)
	}
	if _, err := v.Verify(signRS(t, testKeys(t)[1], "", claims(nil))); err == nil {
		t.Fatal("token signed by another key accepted")
	}
	// Algorithm confusion: an HS256 token "signed" with the public key bytes.
	pemBytes, _ := os.ReadFile(path)
	forged, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims(nil)).SignedString(pemBytes)
	if _, err := v.Verify(forged); err == nil {
		t.Fatal("HS256 token accepted by RS256 provider")
	}
}

// jwksServer serves the given keys and counts fetches.
type jwksServer struct {
	*httptest.Server
	mu   sync.Mutex
	keys map[string]*rsa.PublicKey
	hits atomic.Int32
	down atomic.Bool
}

func newJWKSServer(t *testing.T, keys map[string]*rsa.PublicKey) *jwksServer {
	s := &jwksServer{keys: keys}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.hits.Add(1)
		if s.down.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		set := map[string][]map[string]string{"keys": {}}
		for kid, k := range s.keys {
			set["keys"] = append(set["keys"], EncodeJWK(kid, k))
		}
		_ = json.NewEncoder(w).Encode(set)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *jwksServer) setKeys(keys map[string]*rsa.PublicKey) {
	s.mu.Lock()
	s.keys = keys
	s.mu.Unlock()
}

func TestJWKSCachesAndHandlesRotation(t *testing.T) {
	k := testKeys(t)
	srv := newJWKSServer(t, map[string]*rsa.PublicKey{"2026-01": &k[0].PublicKey})
	now := time.Unix(1_700_000_000, 0)
	jwks := NewJWKS(srv.URL, 10*time.Minute)
	jwks.Now = func() time.Time { return now }
	v, err := NewJWTVerifier(config.JWTProvider{Algorithms: []string{"RS256"}, JWKSURL: srv.URL}, jwks)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		if _, err := v.Verify(signRS(t, k[0], "2026-01", claims(nil))); err != nil {
			t.Fatal(err)
		}
	}
	if srv.hits.Load() != 1 {
		t.Fatalf("JWKS fetched %d times for one kid, want 1 (cached)", srv.hits.Load())
	}

	// The IdP rotates to a new key; the first token with the new kid refreshes.
	srv.setKeys(map[string]*rsa.PublicKey{"2026-01": &k[0].PublicKey, "2026-07": &k[1].PublicKey})
	now = now.Add(time.Minute)
	if _, err := v.Verify(signRS(t, k[1], "2026-07", claims(nil))); err != nil {
		t.Fatalf("rotated key not picked up: %v", err)
	}
	if srv.hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2", srv.hits.Load())
	}

	// Unknown kids are throttled: no refetch inside MinRefresh.
	for i := 0; i < 10; i++ {
		if _, err := v.Verify(signRS(t, k[1], "random-kid", claims(nil))); err == nil {
			t.Fatal("unknown kid accepted")
		}
	}
	if srv.hits.Load() != 2 {
		t.Fatalf("unknown kids caused %d extra fetches", srv.hits.Load()-2)
	}
	if _, err := v.Verify(signRS(t, k[1], "", claims(nil))); err == nil {
		t.Fatal("token without kid accepted by a JWKS provider")
	}
}

func TestJWKSServesStaleKeysWhenIdPIsDown(t *testing.T) {
	k := testKeys(t)
	srv := newJWKSServer(t, map[string]*rsa.PublicKey{"a": &k[0].PublicKey})
	now := time.Unix(1_700_000_000, 0)
	jwks := NewJWKS(srv.URL, time.Minute)
	jwks.Now = func() time.Time { return now }
	if _, err := jwks.Key("a"); err != nil {
		t.Fatal(err)
	}
	srv.down.Store(true)
	now = now.Add(time.Hour) // cache expired
	if _, err := jwks.Key("a"); err != nil {
		t.Fatalf("known key not served while IdP down: %v", err)
	}
	if _, err := jwks.Key("b"); err == nil {
		t.Fatal("unknown key resolved while IdP down")
	}
}

func TestRSAFromJWKRejectsWeakKeys(t *testing.T) {
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	jwk := EncodeJWK("x", &small.PublicKey)
	if _, err := rsaFromJWK(jwk["n"], jwk["e"]); err == nil {
		t.Fatal("1024-bit key accepted")
	}
	if _, err := rsaFromJWK("!!", "AQAB"); err == nil {
		t.Fatal("invalid base64 accepted")
	}
}

func TestAPIKeyVerifier(t *testing.T) {
	v, err := NewAPIKeyVerifier(config.APIKeySet{Header: "X-API-Key", Keys: []config.APIKey{
		{Name: "pathao", Key: "plain-key"},
		{Name: "sundarban", SHA256: HashKey("hashed-key")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"plain-key": "key:pathao", "hashed-key": "key:sundarban"} {
		p, err := v.Verify(key)
		if err != nil || p.Consumer != want {
			t.Errorf("Verify(%q) = %+v, %v; want %s", key, p, err, want)
		}
	}
	for _, bad := range []string{"", "plain-key ", "PLAIN-KEY", "nope"} {
		if _, err := v.Verify(bad); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("Verify(%q) accepted", bad)
		}
	}
	if _, err := NewAPIKeyVerifier(config.APIKeySet{Keys: []config.APIKey{{Name: "x", SHA256: "zz"}}}); err == nil {
		t.Fatal("invalid digest accepted")
	}
}

func TestBearerTokenAndClaimString(t *testing.T) {
	for in, want := range map[string]string{"Bearer abc": "abc", "bearer  abc ": "abc", "Basic abc": "", "Bearer": "", "": ""} {
		got, ok := BearerToken(in)
		if got != want || ok != (want != "") {
			t.Errorf("BearerToken(%q) = %q, %v", in, got, ok)
		}
	}
	for _, c := range []struct {
		in   any
		want string
		ok   bool
	}{
		{"admin", "admin", true}, {float64(42), "42", true}, {1.5, "1.5", true}, {true, "true", true},
		{[]any{"a", "b"}, "a,b", true}, {map[string]any{"x": 1}, "", false}, {[]any{map[string]any{}}, "", false},
	} {
		got, ok := ClaimString(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ClaimString(%v) = %q, %v", c.in, got, ok)
		}
	}
}

func BenchmarkVerifyHS256(b *testing.B) {
	v := hsVerifier(b)
	tok := signHS(b, claims(nil))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := v.Verify(tok); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkVerifyRS256JWKS(b *testing.B) {
	k := testKeys(b)
	jwks := NewJWKS("http://unused", time.Hour)
	jwks.keys = map[string]*rsa.PublicKey{"k": &k[0].PublicKey}
	jwks.fetchedAt = time.Now()
	v, _ := NewJWTVerifier(config.JWTProvider{Algorithms: []string{"RS256"}, JWKSURL: "http://unused"}, jwks)
	tok := signRS(b, k[0], "k", claims(nil))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := v.Verify(tok); err != nil {
			b.Fatal(err)
		}
	}
}
