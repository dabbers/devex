package auth

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const testIssuer = "https://above-gobbler-4528.clerk.accounts.dev"

var testNow = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

// signer mints tokens the way an issuer would, so tests can also mint the ones
// an attacker would.
type signer struct {
	key   *rsa.PrivateKey
	keyID string
}

func newSigner(t *testing.T) *signer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return &signer{key: key, keyID: "ins_test"}
}

func (s *signer) keys() staticKeys {
	return staticKeys{s.keyID: &s.key.PublicKey}
}

func encode(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

// sign produces a correctly signed RS256 token over the given header and claims.
func (s *signer) sign(t *testing.T, header, claims map[string]any) string {
	t.Helper()
	signed := encode(t, header) + "." + encode(t, claims)
	digest := sha256.Sum256([]byte(signed))
	signature, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func validHeader(s *signer) map[string]any {
	return map[string]any{"alg": "RS256", "kid": s.keyID, "typ": "JWT"}
}

func validClaims() map[string]any {
	return map[string]any{
		"iss": testIssuer,
		"sub": "user_2abc",
		"sid": "sess_1",
		"exp": testNow.Add(time.Hour).Unix(),
		"nbf": testNow.Add(-time.Minute).Unix(),
		"iat": testNow.Add(-time.Minute).Unix(),
		"azp": "https://dabberz.dab.im",
	}
}

func TestVerifiesAWellFormedToken(t *testing.T) {
	s := newSigner(t)
	token := s.sign(t, validHeader(s), validClaims())

	identity, err := verifyToken(token, s.keys(), verifyOptions{
		Issuer:            testIssuer,
		AuthorizedParties: []string{"https://dabberz.dab.im"},
	}, testNow)
	if err != nil {
		t.Fatalf("a valid token was rejected: %v", err)
	}
	if identity.Subject != "user_2abc" || identity.SessionID != "sess_1" {
		t.Fatalf("identity = %+v", identity)
	}
}

// TestRefusesAlgorithmSubstitution is the attack the allowlist exists for: the
// token is re-signed with HS256 using the RSA public key as the HMAC secret,
// which an implementation that merely rejects "none" accepts.
func TestRefusesAlgorithmSubstitution(t *testing.T) {
	s := newSigner(t)
	publicKeyBytes := s.key.N.Bytes()

	header := map[string]any{"alg": "HS256", "kid": s.keyID, "typ": "JWT"}
	signed := encode(t, header) + "." + encode(t, validClaims())
	mac := hmac.New(sha256.New, publicKeyBytes)
	mac.Write([]byte(signed))
	token := signed + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	_, err := verifyToken(token, s.keys(), verifyOptions{Issuer: testIssuer}, testNow)
	if err == nil {
		t.Fatal("an HS256 token signed with the RSA public key was accepted")
	}
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("error = %v, want ErrUnauthenticated", err)
	}
}

func TestRefusesUnsignedToken(t *testing.T) {
	s := newSigner(t)
	header := map[string]any{"alg": "none", "kid": s.keyID, "typ": "JWT"}
	token := encode(t, header) + "." + encode(t, validClaims()) + "."

	if _, err := verifyToken(token, s.keys(), verifyOptions{Issuer: testIssuer}, testNow); err == nil {
		t.Fatal("an unsigned token was accepted")
	}
}

func TestRefusesATamperedPayload(t *testing.T) {
	s := newSigner(t)
	token := s.sign(t, validHeader(s), validClaims())

	// Swap the claims for ones naming a different subject, keeping the
	// signature. This is the whole point of signing.
	parts := strings.Split(token, ".")
	elevated := validClaims()
	elevated["sub"] = "user_somebody_else"
	parts[1] = encode(t, elevated)

	if _, err := verifyToken(strings.Join(parts, "."), s.keys(),
		verifyOptions{Issuer: testIssuer}, testNow); err == nil {
		t.Fatal("a token with replaced claims was accepted")
	}
}

func TestRefusesAnUnknownKeyID(t *testing.T) {
	s := newSigner(t)
	header := validHeader(s)
	header["kid"] = "ins_not_published"
	token := s.sign(t, header, validClaims())

	if _, err := verifyToken(token, s.keys(), verifyOptions{Issuer: testIssuer}, testNow); err == nil {
		t.Fatal("a token naming an unpublished key was accepted")
	}
}

func TestRefusesAMissingKeyID(t *testing.T) {
	s := newSigner(t)
	header := map[string]any{"alg": "RS256", "typ": "JWT"}
	token := s.sign(t, header, validClaims())

	// Without a key id there is no way to know which key should have signed
	// this, and trying every key is how a retired key stays usable.
	if _, err := verifyToken(token, s.keys(), verifyOptions{Issuer: testIssuer}, testNow); err == nil {
		t.Fatal("a token with no key id was accepted")
	}
}

func TestRefusesTokenFromAnotherIssuer(t *testing.T) {
	s := newSigner(t)
	claims := validClaims()
	claims["iss"] = "https://someone-elses-instance.clerk.accounts.dev"
	token := s.sign(t, validHeader(s), claims)

	if _, err := verifyToken(token, s.keys(), verifyOptions{Issuer: testIssuer}, testNow); err == nil {
		t.Fatal("a token from another issuer was accepted")
	}
}

func TestRefusesAnExpiredToken(t *testing.T) {
	s := newSigner(t)
	claims := validClaims()
	claims["exp"] = testNow.Add(-time.Hour).Unix()
	token := s.sign(t, validHeader(s), claims)

	if _, err := verifyToken(token, s.keys(), verifyOptions{Issuer: testIssuer}, testNow); err == nil {
		t.Fatal("an expired token was accepted")
	}
}

// TestRefusesATokenWithNoExpiry: a token that never expires cannot be revoked
// by waiting, so absence of exp is a rejection rather than a licence.
func TestRefusesATokenWithNoExpiry(t *testing.T) {
	s := newSigner(t)
	claims := validClaims()
	delete(claims, "exp")
	token := s.sign(t, validHeader(s), claims)

	if _, err := verifyToken(token, s.keys(), verifyOptions{Issuer: testIssuer}, testNow); err == nil {
		t.Fatal("a token with no expiry was accepted")
	}
}

func TestRefusesATokenThatIsNotYetValid(t *testing.T) {
	s := newSigner(t)
	claims := validClaims()
	claims["nbf"] = testNow.Add(time.Hour).Unix()
	token := s.sign(t, validHeader(s), claims)

	if _, err := verifyToken(token, s.keys(), verifyOptions{Issuer: testIssuer}, testNow); err == nil {
		t.Fatal("a token that is not yet valid was accepted")
	}
}

func TestToleratesSmallClockSkew(t *testing.T) {
	s := newSigner(t)
	claims := validClaims()
	// Expired a handful of seconds ago by this machine's clock. Refusing this
	// would sign people out over ordinary clock drift.
	claims["exp"] = testNow.Add(-5 * time.Second).Unix()
	token := s.sign(t, validHeader(s), claims)

	if _, err := verifyToken(token, s.keys(), verifyOptions{Issuer: testIssuer}, testNow); err != nil {
		t.Fatalf("a token within the clock leeway was rejected: %v", err)
	}
}

// TestRefusesATokenMintedForAnotherApplication: Clerk instances can serve
// several origins, and a token issued to one must not be replayable at another.
func TestRefusesATokenMintedForAnotherApplication(t *testing.T) {
	s := newSigner(t)
	claims := validClaims()
	claims["azp"] = "https://someone-elses-app.example.com"
	token := s.sign(t, validHeader(s), claims)

	_, err := verifyToken(token, s.keys(), verifyOptions{
		Issuer:            testIssuer,
		AuthorizedParties: []string{"https://dabberz.dab.im"},
	}, testNow)
	if err == nil {
		t.Fatal("a token issued to another origin was accepted")
	}
}

func TestRefusesATokenWithNoSubject(t *testing.T) {
	s := newSigner(t)
	claims := validClaims()
	delete(claims, "sub")
	token := s.sign(t, validHeader(s), claims)

	if _, err := verifyToken(token, s.keys(), verifyOptions{Issuer: testIssuer}, testNow); err == nil {
		t.Fatal("a token naming nobody was accepted")
	}
}

func TestRefusesMalformedTokens(t *testing.T) {
	s := newSigner(t)
	for _, token := range []string{
		"",
		"not-a-token",
		"only.two",
		"a.b.c",
		"...",
		strings.Repeat("x", maxTokenBytes+1),
	} {
		if _, err := verifyToken(token, s.keys(), verifyOptions{Issuer: testIssuer}, testNow); err == nil {
			t.Fatalf("malformed token %q was accepted", truncateForMessage(token))
		}
	}
}

func TestAudienceAcceptsStringOrList(t *testing.T) {
	s := newSigner(t)
	for name, aud := range map[string]any{
		"string": "dabberz",
		"list":   []string{"other", "dabberz"},
	} {
		claims := validClaims()
		claims["aud"] = aud
		token := s.sign(t, validHeader(s), claims)
		if _, err := verifyToken(token, s.keys(),
			verifyOptions{Issuer: testIssuer, Audience: "dabberz"}, testNow); err != nil {
			t.Fatalf("aud as a %s was rejected: %v", name, err)
		}
	}

	claims := validClaims()
	claims["aud"] = []string{"somebody-else"}
	token := s.sign(t, validHeader(s), claims)
	if _, err := verifyToken(token, s.keys(),
		verifyOptions{Issuer: testIssuer, Audience: "dabberz"}, testNow); err == nil {
		t.Fatal("a token for another audience was accepted")
	}
}

func truncateForMessage(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}
