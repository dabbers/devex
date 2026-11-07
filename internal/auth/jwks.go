package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// keyLookup resolves a key id to the public key that signs with it.
type keyLookup interface {
	key(keyID string) (*rsa.PublicKey, error)
}

// How long a fetched key set is trusted, and how often an unknown key id is
// allowed to force an early refetch.
//
// Providers rotate signing keys, so an unknown key id is an ordinary event and
// has to trigger a refresh. It is also exactly what an attacker would send to
// make this process fetch a URL on demand, so the refresh is rate limited and
// a token with an unknown id is refused in the meantime rather than queued.
const (
	keySetTTL          = time.Hour
	keySetMinRefetch   = time.Minute
	keySetFetchTimeout = 10 * time.Second
	maxKeySetBytes     = 1 << 20
)

// keySet caches the signing keys published by an identity provider.
type keySet struct {
	url    string
	client *http.Client

	mu          sync.Mutex
	keys        map[string]*rsa.PublicKey
	fetchedAt   time.Time
	lastAttempt time.Time
	now         func() time.Time
}

// newKeySet returns a key set that fetches from url on first use.
func newKeySet(url string, client *http.Client) *keySet {
	if client == nil {
		client = &http.Client{Timeout: keySetFetchTimeout}
	}
	return &keySet{url: url, client: client, keys: map[string]*rsa.PublicKey{}, now: time.Now}
}

// key returns the public key for a key id, refetching the set if the id is
// unknown or the cache has aged out.
func (k *keySet) key(keyID string) (*rsa.PublicKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	now := k.now()
	fresh := now.Sub(k.fetchedAt) < keySetTTL
	if found, ok := k.keys[keyID]; ok && fresh {
		return found, nil
	}

	// Refetch, unless one was attempted moments ago. Without that guard a
	// stream of tokens carrying invented key ids becomes a stream of outbound
	// requests.
	if now.Sub(k.lastAttempt) < keySetMinRefetch {
		if found, ok := k.keys[keyID]; ok {
			return found, nil
		}
		return nil, fmt.Errorf("%w: no signing key %q, and the key set was just refreshed",
			ErrUnauthenticated, keyID)
	}
	k.lastAttempt = now

	ctx, cancel := context.WithTimeout(context.Background(), keySetFetchTimeout)
	defer cancel()
	fetched, err := k.fetch(ctx)
	if err != nil {
		// Keep serving the cached keys. A provider outage should not sign
		// everybody out of a system that already holds valid keys.
		if found, ok := k.keys[keyID]; ok {
			return found, nil
		}
		return nil, fmt.Errorf("%w: could not fetch signing keys: %w", ErrUnauthenticated, err)
	}
	k.keys = fetched
	k.fetchedAt = now

	found, ok := k.keys[keyID]
	if !ok {
		return nil, fmt.Errorf("%w: the provider publishes no signing key %q",
			ErrUnauthenticated, keyID)
	}
	return found, nil
}

// jwksDocument is the published key set.
type jwksDocument struct {
	Keys []struct {
		KeyType   string `json:"kty"`
		KeyID     string `json:"kid"`
		Algorithm string `json:"alg"`
		Use       string `json:"use"`
		Modulus   string `json:"n"`
		Exponent  string `json:"e"`
	} `json:"keys"`
}

// fetch reads and parses the key set.
func (k *keySet) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("key set returned %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxKeySetBytes))
	if err != nil {
		return nil, err
	}
	var doc jwksDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("key set is not readable JSON: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, entry := range doc.Keys {
		// Only RSA signing keys are usable, and only RS256 is accepted, so
		// anything else is skipped rather than stored and rejected later.
		if entry.KeyType != "RSA" || entry.KeyID == "" {
			continue
		}
		if entry.Use != "" && entry.Use != "sig" {
			continue
		}
		if entry.Algorithm != "" && entry.Algorithm != "RS256" {
			continue
		}
		publicKey, err := rsaKey(entry.Modulus, entry.Exponent)
		if err != nil {
			// One malformed key must not discard the rest of the set.
			continue
		}
		keys[entry.KeyID] = publicKey
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("key set contains no usable RS256 signing keys")
	}
	return keys, nil
}

// rsaKey rebuilds a public key from its base64url modulus and exponent.
func rsaKey(modulus, exponent string) (*rsa.PublicKey, error) {
	n, err := base64.RawURLEncoding.DecodeString(modulus)
	if err != nil {
		return nil, fmt.Errorf("modulus is not base64url: %w", err)
	}
	e, err := base64.RawURLEncoding.DecodeString(exponent)
	if err != nil {
		return nil, fmt.Errorf("exponent is not base64url: %w", err)
	}
	if len(n) == 0 || len(e) == 0 {
		return nil, fmt.Errorf("modulus or exponent is empty")
	}
	// A short modulus means a weak key. 2048 bits is the floor worth
	// accepting, and providers publish 2048 or more.
	if len(n)*8 < 2048 {
		return nil, fmt.Errorf("modulus is %d bits, under the 2048-bit minimum", len(n)*8)
	}
	key := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	if key.E < 3 {
		return nil, fmt.Errorf("public exponent %d is too small", key.E)
	}
	return key, nil
}

// staticKeys is a keyLookup over a fixed map, for tests.
type staticKeys map[string]*rsa.PublicKey

func (s staticKeys) key(keyID string) (*rsa.PublicKey, error) {
	found, ok := s[keyID]
	if !ok {
		return nil, fmt.Errorf("%w: no signing key %q", ErrUnauthenticated, keyID)
	}
	return found, nil
}

var (
	_ keyLookup = (*keySet)(nil)
	_ keyLookup = staticKeys(nil)
)
