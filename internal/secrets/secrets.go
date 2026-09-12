// Package secrets stores per-repo secrets encrypted at rest.
//
// The repo is the root of the hierarchy: secrets are set once on a repo and
// inherited by every project and fork beneath it, so a new workstream starts
// with the credentials it needs without anyone copying them around. Values are
// sealed with AES-256-GCM and only ever decrypted on the way into a fork's VM.
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"

	"github.com/dabbers/devex/internal/store"
)

// KeySize is the master key length in bytes (AES-256).
const KeySize = 32

// ErrNotFound reports an unknown secret.
var ErrNotFound = errors.New("secrets: not found")

// namePattern constrains secret names to what a POSIX shell will carry as an
// environment variable, since that is how they reach the agent.
var namePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// ValidateName checks a secret name.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("secrets: %q is not a valid name; use upper-case letters, digits and underscores", name)
	}
	return nil
}

// GenerateKey returns a new hex-encoded master key.
func GenerateKey() (string, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("secrets: generate key: %w", err)
	}
	return hex.EncodeToString(key), nil
}

// ParseKey decodes a hex-encoded master key.
func ParseKey(encoded string) ([]byte, error) {
	key, err := hex.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("secrets: master key is not valid hex: %w", err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("secrets: master key is %d bytes, want %d", len(key), KeySize)
	}
	return key, nil
}

// Vault seals and opens repo-scoped secrets.
type Vault struct {
	db   *store.Store
	aead cipher.AEAD
}

// NewVault returns a vault sealing with the given master key.
func NewVault(db *store.Store, key []byte) (*Vault, error) {
	if db == nil {
		return nil, errors.New("secrets: a store is required")
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("secrets: master key is %d bytes, want %d", len(key), KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: initialise cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: initialise AEAD: %w", err)
	}
	return &Vault{db: db, aead: aead}, nil
}

// Set stores a secret on a repo, replacing any previous value.
func (v *Vault) Set(ctx context.Context, repoID, name, value string) error {
	if repoID == "" {
		return errors.New("secrets: a repo id is required")
	}
	if err := ValidateName(name); err != nil {
		return err
	}

	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("secrets: generate nonce: %w", err)
	}
	// The repo and name are authenticated but not encrypted, so a sealed value
	// cannot be moved to another repo or renamed without detection.
	ciphertext := v.aead.Seal(nil, nonce, []byte(value), associatedData(repoID, name))

	return v.db.PutSecret(ctx, &store.SecretRow{
		RepoID:     repoID,
		Name:       name,
		Nonce:      nonce,
		Ciphertext: ciphertext,
	})
}

// Get returns one decrypted secret.
func (v *Vault) Get(ctx context.Context, repoID, name string) (string, error) {
	row, err := v.db.GetSecret(ctx, repoID, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("secrets: %s in repo %s: %w", name, repoID, ErrNotFound)
		}
		return "", err
	}
	return v.open(row)
}

// Names lists the secret names set on a repo. Values are deliberately not
// returned: listing is what the UI calls, and it has no business decrypting.
func (v *Vault) Names(ctx context.Context, repoID string) ([]string, error) {
	rows, err := v.db.ListSecrets(ctx, repoID)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Name)
	}
	return names, nil
}

// Delete removes a secret from a repo.
func (v *Vault) Delete(ctx context.Context, repoID, name string) error {
	err := v.db.DeleteSecret(ctx, repoID, name)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("secrets: %s in repo %s: %w", name, repoID, ErrNotFound)
	}
	return err
}

// Environment returns every secret on a repo, decrypted, ready to inject into
// a fork's VM. This is the inheritance step: a fork receives its repo's whole
// secret set without naming any of them.
func (v *Vault) Environment(ctx context.Context, repoID string) (map[string]string, error) {
	rows, err := v.db.ListSecrets(ctx, repoID)
	if err != nil {
		return nil, err
	}
	env := make(map[string]string, len(rows))
	for _, row := range rows {
		value, err := v.open(row)
		if err != nil {
			return nil, err
		}
		env[row.Name] = value
	}
	return env, nil
}

// open decrypts one stored row.
func (v *Vault) open(row *store.SecretRow) (string, error) {
	plaintext, err := v.aead.Open(nil, row.Nonce, row.Ciphertext, associatedData(row.RepoID, row.Name))
	if err != nil {
		// This means the master key changed or the row was tampered with.
		// Either way the value is unrecoverable and must not be guessed at.
		return "", fmt.Errorf("secrets: cannot decrypt %s in repo %s; the master key may have changed: %w", row.Name, row.RepoID, err)
	}
	return string(plaintext), nil
}

// associatedData binds a sealed value to its repo and name.
func associatedData(repoID, name string) []byte {
	return []byte(repoID + "\x00" + name)
}
