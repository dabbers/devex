// Package previewpass lets the platform look at one preview as its owner, for
// a few minutes, without holding anything that works anywhere else.
//
// The browser check exists to see a change the way the person who asked for
// it will. For an app whose front page is behind sign-in -- Forms' is its
// list of forms -- an anonymous visit sees only "Sign in", and the check
// could never pass: on dbz-alpha it stopped at a 401 before a browser ever
// started, so no change could be promoted at all.
//
// The obvious credential is the owner's platform token, and it is the wrong
// one. That token is the whole account, and the page being checked runs code
// an agent wrote minutes ago. A pass is narrower in every direction that
// matters: it names one preview hostname, one person and an expiry; the edge
// accepts it only on that hostname and strips it before the request reaches
// the app; the verifier sets it as a host-only cookie, so the browser never
// sends it to anything else the page loads; and it is signed with a key that
// exists only in the running daemon, so it does not outlive a restart.
package previewpass

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dabbers/devex/internal/domain"
)

// CookieName is what a pass travels as. Prefixed like everything else the
// platform asserts, so it is plainly not the app's own.
const CookieName = "dabberz_preview_pass"

// MinKeySize is the smallest signing key accepted.
const MinKeySize = 32

var (
	// ErrInvalid means the pass was not one this daemon issued, or was altered.
	ErrInvalid = errors.New("previewpass: not a valid pass")
	// ErrExpired means the pass was genuine and is out of date.
	ErrExpired = errors.New("previewpass: the pass has expired")
	// ErrWrongHost means the pass is genuine and for a different preview.
	ErrWrongHost = errors.New("previewpass: the pass is for a different preview")
)

// Issuer mints passes and checks them.
type Issuer struct {
	key []byte
	now func() time.Time
}

// New returns an issuer signing with key.
func New(key []byte) (*Issuer, error) {
	if len(key) < MinKeySize {
		return nil, fmt.Errorf("previewpass: a key of at least %d bytes is required", MinKeySize)
	}
	return &Issuer{key: append([]byte(nil), key...), now: time.Now}, nil
}

// NewRandom returns an issuer with a key nobody else has. Passes it mints are
// good only until the process exits, which is longer than any check runs.
func NewRandom() (*Issuer, error) {
	key := make([]byte, MinKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("previewpass: no randomness available: %w", err)
	}
	return New(key)
}

// claims is what a pass says. Short names: it rides in a cookie.
type claims struct {
	Host    string `json:"h"`
	UserID  string `json:"u"`
	Email   string `json:"m,omitempty"`
	Expires int64  `json:"x"`
}

// Mint returns a pass letting user be recognised at host until ttl from now.
func (i *Issuer) Mint(host string, user *domain.User, ttl time.Duration) (string, error) {
	host = normalize(host)
	switch {
	case host == "":
		return "", errors.New("previewpass: a hostname is required")
	case user == nil || user.ID == "":
		return "", errors.New("previewpass: a user is required")
	case ttl <= 0:
		return "", errors.New("previewpass: a positive lifetime is required")
	}
	raw, err := json.Marshal(claims{
		Host: host, UserID: user.ID, Email: user.Email,
		Expires: i.now().Add(ttl).Unix(),
	})
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	return body + "." + base64.RawURLEncoding.EncodeToString(i.sign(body)), nil
}

// Check returns who a pass names, if it is genuine, current, and for host.
func (i *Issuer) Check(presented, host string) (*domain.User, error) {
	body, signature, ok := strings.Cut(presented, ".")
	if !ok {
		return nil, ErrInvalid
	}
	got, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || !hmac.Equal(got, i.sign(body)) {
		return nil, ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, ErrInvalid
	}
	var c claims
	if err := json.Unmarshal(raw, &c); err != nil || c.UserID == "" || c.Host == "" {
		return nil, ErrInvalid
	}
	// Signature first, then the claims: an unsigned pass says nothing worth
	// reporting back, so it is never told whether its host or date was right.
	if i.now().Unix() >= c.Expires {
		return nil, ErrExpired
	}
	if c.Host != normalize(host) {
		return nil, ErrWrongHost
	}
	return &domain.User{ID: c.UserID, Email: c.Email}, nil
}

func (i *Issuer) sign(body string) []byte {
	mac := hmac.New(sha256.New, i.key)
	mac.Write([]byte(body)) //nolint:errcheck // hash writes never fail
	return mac.Sum(nil)
}

func normalize(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, ok := strings.Cut(host, ":"); ok {
		return h
	}
	return host
}
