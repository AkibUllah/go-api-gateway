package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/AkibUllah/go-api-gateway/internal/config"
)

// APIKeyVerifier checks a header against a set of keys. Keys are stored only as
// SHA-256 digests and compared in constant time.
type APIKeyVerifier struct {
	Header string
	keys   []hashedKey
}

type hashedKey struct {
	name   string
	digest [sha256.Size]byte
}

// NewAPIKeyVerifier builds a verifier from configuration.
func NewAPIKeyVerifier(set config.APIKeySet) (*APIKeyVerifier, error) {
	v := &APIKeyVerifier{Header: set.Header}
	for _, k := range set.Keys {
		var hk hashedKey
		hk.name = k.Name
		if k.Key != "" {
			hk.digest = sha256.Sum256([]byte(k.Key))
		} else {
			b, err := hex.DecodeString(strings.ToLower(k.SHA256))
			if err != nil || len(b) != sha256.Size {
				return nil, fmt.Errorf("api key %q: invalid sha256", k.Name)
			}
			copy(hk.digest[:], b)
		}
		v.keys = append(v.keys, hk)
	}
	return v, nil
}

// Verify returns the principal for a presented key. Every stored key is
// compared so timing does not reveal which entry (if any) matched.
func (v *APIKeyVerifier) Verify(presented string) (*Principal, error) {
	if presented == "" {
		return nil, fmt.Errorf("%w: missing api key", ErrUnauthorized)
	}
	d := sha256.Sum256([]byte(presented))
	match := ""
	for _, k := range v.keys {
		if subtle.ConstantTimeCompare(d[:], k.digest[:]) == 1 {
			match = k.name
		}
	}
	if match == "" {
		return nil, fmt.Errorf("%w: invalid api key", ErrUnauthorized)
	}
	return &Principal{Subject: match, Consumer: "key:" + match}, nil
}

// HashKey returns the hex SHA-256 digest of a key, for writing configs.
func HashKey(key string) string {
	d := sha256.Sum256([]byte(key))
	return hex.EncodeToString(d[:])
}
