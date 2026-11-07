package auth

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Verification is written by hand rather than pulled in, because the project
// keeps its dependency surface small and because the dangerous parts of JWT
// are the parts a library hides: which algorithms are accepted and which
// claims are actually checked. Both are stated here, in the open.
//
// The rules, all of them mandatory:
//
//   - The algorithm must be exactly RS256. This is an allowlist, not a check
//     for "alg: none" -- the attack is to re-sign a token with HS256 using the
//     RSA public key as the HMAC secret, which a denylist lets through.
//   - A key id must be present and must name a key in the published set. No
//     trying every key, and no embedded keys from the token's own header.
//   - The issuer must match exactly.
//   - An expiry must be present. A token with no exp is refused rather than
//     treated as eternal.
//
// Anything not on that list is refused.

// maxTokenBytes bounds what will be parsed at all, so a large body cannot be
// turned into work before any signature is checked.
const maxTokenBytes = 8192

// clockLeeway absorbs small differences between this machine's clock and the
// issuer's. It is deliberately small: a generous window extends the life of
// every revoked token.
const clockLeeway = 30 * time.Second

// jwtHeader is the part of a token's header this code is willing to read.
type jwtHeader struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Type      string `json:"typ"`
}

// jwtClaims is the subset of claims dabberz uses or validates.
type jwtClaims struct {
	Issuer    string   `json:"iss"`
	Subject   string   `json:"sub"`
	Audience  audience `json:"aud"`
	Expiry    *int64   `json:"exp"`
	NotBefore *int64   `json:"nbf"`
	IssuedAt  *int64   `json:"iat"`
	// AuthorizedParty is the origin the token was minted for. Checking it is
	// what stops a token issued to another application on the same identity
	// provider from being replayed here.
	AuthorizedParty string `json:"azp"`
	SessionID       string `json:"sid"`
	Email           string `json:"email"`
	// Status is Clerk's session status on v2 tokens. A token can be minted for
	// a session that is not usable -- pending factor-two, for instance -- and
	// the signature says nothing about that.
	Status string `json:"sts"`
}

// audience decodes the aud claim, which the spec allows to be either a string
// or an array of strings.
type audience []string

// UnmarshalJSON implements json.Unmarshaler.
func (a *audience) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*a = audience{single}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("auth: aud is neither a string nor a list of strings")
	}
	*a = many
	return nil
}

func (a audience) contains(want string) bool {
	for _, got := range a {
		if got == want {
			return true
		}
	}
	return false
}

// verifyOptions is what a token is checked against.
type verifyOptions struct {
	// Issuer is required and compared exactly.
	Issuer string
	// Audience, when set, must appear in the token's aud claim. Left empty the
	// claim is not checked, because not every provider sets it on a session
	// token.
	Audience string
	// AuthorizedParties, when non-empty, restricts the azp claim to this set.
	//
	// Opt-in, because only a token minted by a browser carries azp: one minted
	// through a provider's backend API has no such claim and would be refused.
	// It is worth enabling when one identity-provider instance serves several
	// applications, which is the case it defends against.
	AuthorizedParties []string
}

// verifyToken checks a token's signature and claims and returns the identity
// it asserts.
func verifyToken(token string, keys keyLookup, opts verifyOptions, now time.Time) (*Identity, error) {
	if len(token) > maxTokenBytes {
		return nil, fmt.Errorf("%w: token is %d bytes, over the %d-byte limit",
			ErrUnauthenticated, len(token), maxTokenBytes)
	}
	signedPart, encodedSignature, ok := lastDot(token)
	if !ok {
		return nil, fmt.Errorf("%w: token is not three dot-separated parts", ErrUnauthenticated)
	}
	encodedHeader, encodedClaims, ok := strings.Cut(signedPart, ".")
	if !ok || encodedHeader == "" || encodedClaims == "" {
		return nil, fmt.Errorf("%w: token is not three dot-separated parts", ErrUnauthenticated)
	}

	headerBytes, err := decodeSegment(encodedHeader)
	if err != nil {
		return nil, fmt.Errorf("%w: unreadable header: %w", ErrUnauthenticated, err)
	}
	var header jwtHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, fmt.Errorf("%w: unreadable header: %w", ErrUnauthenticated, err)
	}

	// The allowlist. Refusing anything but RS256 is what makes the signature
	// check meaningful; see the note at the top of this file.
	if header.Algorithm != "RS256" {
		return nil, fmt.Errorf("%w: algorithm %q is not accepted, only RS256",
			ErrUnauthenticated, header.Algorithm)
	}
	if header.Type != "" && !strings.EqualFold(header.Type, "JWT") {
		return nil, fmt.Errorf("%w: token type %q is not accepted", ErrUnauthenticated, header.Type)
	}
	if header.KeyID == "" {
		return nil, fmt.Errorf("%w: token names no key id", ErrUnauthenticated)
	}

	publicKey, err := keys.key(header.KeyID)
	if err != nil {
		return nil, err
	}

	signature, err := decodeSegment(encodedSignature)
	if err != nil {
		return nil, fmt.Errorf("%w: unreadable signature: %w", ErrUnauthenticated, err)
	}
	digest := sha256.Sum256([]byte(signedPart))
	if err := rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], signature); err != nil {
		return nil, fmt.Errorf("%w: signature does not verify", ErrUnauthenticated)
	}

	claimBytes, err := decodeSegment(encodedClaims)
	if err != nil {
		return nil, fmt.Errorf("%w: unreadable claims: %w", ErrUnauthenticated, err)
	}
	var claims jwtClaims
	if err := json.Unmarshal(claimBytes, &claims); err != nil {
		return nil, fmt.Errorf("%w: unreadable claims: %w", ErrUnauthenticated, err)
	}

	if err := validateClaims(&claims, opts, now); err != nil {
		return nil, err
	}
	return &Identity{
		Subject:   claims.Subject,
		Email:     strings.TrimSpace(claims.Email),
		SessionID: claims.SessionID,
	}, nil
}

// validateClaims applies every claim rule. The signature only proves the
// issuer minted these bytes; it says nothing about when, for whom, or for
// which application.
func validateClaims(claims *jwtClaims, opts verifyOptions, now time.Time) error {
	if claims.Issuer != opts.Issuer {
		return fmt.Errorf("%w: issuer %q is not %q", ErrUnauthenticated, claims.Issuer, opts.Issuer)
	}
	if claims.Subject == "" {
		return fmt.Errorf("%w: token names no subject", ErrUnauthenticated)
	}
	// A token with no expiry never stops being valid, so it is refused rather
	// than accepted as a token that does not expire.
	if claims.Expiry == nil {
		return fmt.Errorf("%w: token has no expiry", ErrUnauthenticated)
	}
	if now.After(time.Unix(*claims.Expiry, 0).Add(clockLeeway)) {
		return fmt.Errorf("%w: token expired at %s", ErrUnauthenticated,
			time.Unix(*claims.Expiry, 0).UTC().Format(time.RFC3339))
	}
	if claims.NotBefore != nil && now.Before(time.Unix(*claims.NotBefore, 0).Add(-clockLeeway)) {
		return fmt.Errorf("%w: token is not valid until %s", ErrUnauthenticated,
			time.Unix(*claims.NotBefore, 0).UTC().Format(time.RFC3339))
	}
	if claims.IssuedAt != nil && now.Before(time.Unix(*claims.IssuedAt, 0).Add(-clockLeeway)) {
		return fmt.Errorf("%w: token was issued in the future, at %s", ErrUnauthenticated,
			time.Unix(*claims.IssuedAt, 0).UTC().Format(time.RFC3339))
	}
	// Only checked when present: not every issuer sets it, and absence must
	// not be read as "active".
	if claims.Status != "" && claims.Status != "active" {
		return fmt.Errorf("%w: the session is %q, not active", ErrUnauthenticated, claims.Status)
	}
	if opts.Audience != "" && !claims.Audience.contains(opts.Audience) {
		return fmt.Errorf("%w: token audience %v does not include %q",
			ErrUnauthenticated, []string(claims.Audience), opts.Audience)
	}
	if len(opts.AuthorizedParties) > 0 {
		allowed := false
		for _, party := range opts.AuthorizedParties {
			if claims.AuthorizedParty == party {
				allowed = true
				break
			}
		}
		if !allowed {
			// A token minted for a different application on the same identity
			// provider would otherwise verify here.
			return fmt.Errorf("%w: token was issued to %q, which is not an allowed origin",
				ErrUnauthenticated, claims.AuthorizedParty)
		}
	}
	return nil
}

// lastDot splits a token into the signed portion and the signature, cutting at
// the final separator so the signed bytes are exactly what was signed.
func lastDot(token string) (string, string, bool) {
	index := strings.LastIndex(token, ".")
	if index <= 0 || index == len(token)-1 {
		return "", "", false
	}
	return token[:index], token[index+1:], true
}

// decodeSegment decodes unpadded base64url, which is what JWT uses.
func decodeSegment(segment string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(segment)
}
