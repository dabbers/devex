// Package id generates prefixed, lexicographically sortable identifiers.
//
// An identifier looks like "fork_0004k2m9x8q3v7bntp2wz1cgae": a short type
// prefix, an underscore, then a 26-character base32 encoding of a 48-bit
// millisecond timestamp followed by 80 bits of randomness. Sorting a set of
// identifiers of the same type therefore orders them by creation time, which
// keeps listings stable without a secondary sort key.
package id

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
	"time"
)

// Type prefixes for every persisted entity.
const (
	User      = "usr"
	Repo      = "repo"
	Project   = "proj"
	Task      = "task"
	Fork      = "fork"
	Event     = "evt"
	Instance  = "vm"
	Lease     = "lease"
	Finding   = "find"
	Secret    = "sec"
	Profile   = "prof"
	Question  = "qst"
	Workspace = "ws"
)

// encoding is Crockford-style base32 in lowercase without padding, chosen so
// identifiers stay copy-pasteable and case-insensitively unambiguous.
var encoding = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

// New returns a fresh identifier carrying the given type prefix.
func New(prefix string) string {
	var buf [16]byte
	putMillis(buf[:6], time.Now().UTC().UnixMilli())
	if _, err := rand.Read(buf[6:]); err != nil {
		panic("id: entropy source unavailable: " + err.Error())
	}
	return prefix + "_" + encoding.EncodeToString(buf[:])
}

// putMillis writes ms as a 48-bit big-endian integer, which covers timestamps
// out to the year 10889 and keeps the time bits ahead of the random bits so
// that encoded identifiers sort chronologically.
func putMillis(dst []byte, ms int64) {
	for i := range 6 {
		dst[i] = byte(ms >> (8 * (5 - i)))
	}
}

// millis reads back a 48-bit big-endian integer written by putMillis.
func millis(src []byte) int64 {
	var ms int64
	for i := range 6 {
		ms = ms<<8 | int64(src[i])
	}
	return ms
}

// Prefix returns the type prefix of an identifier, or "" if it has none.
func Prefix(v string) string {
	prefix, _, ok := strings.Cut(v, "_")
	if !ok {
		return ""
	}
	return prefix
}

// Valid reports whether v is a well-formed identifier of the given type.
func Valid(v, prefix string) bool {
	rest, ok := strings.CutPrefix(v, prefix+"_")
	if !ok || len(rest) != 26 {
		return false
	}
	_, err := encoding.DecodeString(rest)
	return err == nil
}

// Time recovers the creation timestamp encoded in an identifier. The zero time
// is returned when v is not a well-formed identifier.
func Time(v string) time.Time {
	_, rest, ok := strings.Cut(v, "_")
	if !ok {
		return time.Time{}
	}
	raw, err := encoding.DecodeString(rest)
	if err != nil || len(raw) != 16 {
		return time.Time{}
	}
	return time.UnixMilli(millis(raw[:6])).UTC()
}
