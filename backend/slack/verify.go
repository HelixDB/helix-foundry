package slack

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// MaxSkew is how far a request's timestamp may be from now, either way.
const MaxSkew = 5 * time.Minute

var (
	// ErrMalformed is a signature or timestamp header that isn't one: a
	// signature other than "v0=" and 64 hex digits, or a timestamp other
	// than Unix seconds.
	ErrMalformed = errors.New("slack: malformed signature headers")
	// ErrStale is a timestamp more than MaxSkew from now: a replay, or a
	// clock far off.
	ErrStale = errors.New("slack: request timestamp outside the window")
	// ErrBadSignature is a well-formed signature that doesn't match.
	ErrBadSignature = errors.New("slack: signature mismatch")
)

// Precheck refuses headers no signed request from Slack carries: a
// malformed signature or timestamp (ErrMalformed), or a timestamp more than
// MaxSkew from now (ErrStale). It needs no secret, so the door runs it
// before it forwards a request; Verify runs it again.
func Precheck(timestamp, signature string, now time.Time) error {
	digest, ok := strings.CutPrefix(signature, "v0=")
	if !ok || len(digest) != sha256.Size*2 || strings.ToLower(digest) != digest {
		return ErrMalformed
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return ErrMalformed
	}
	if timestamp == "" || len(timestamp) > 12 || strings.TrimLeft(timestamp, "0123456789") != "" {
		return ErrMalformed
	}
	seconds, _ := strconv.ParseInt(timestamp, 10, 64) // at most twelve digits: always parses
	if skew := now.Sub(time.Unix(seconds, 0)); skew > MaxSkew || skew < -MaxSkew {
		return ErrStale
	}
	return nil
}

// Sign is the signature Slack sends with body at timestamp:
// "v0=" + hex(HMAC-SHA256(secret, "v0:" + timestamp + ":" + body)).
func Sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("v0:" + timestamp + ":"))
	_, _ = mac.Write(body)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks that Slack signed body at timestamp with the app's signing
// secret, within MaxSkew of now, comparing in constant time. It refuses an
// empty secret, so a misconfigured app accepts nothing.
func Verify(secret, timestamp string, body []byte, signature string, now time.Time) error {
	if err := Precheck(timestamp, signature, now); err != nil {
		return err
	}
	if secret == "" || !hmac.Equal([]byte(Sign(secret, timestamp, body)), []byte(signature)) {
		return ErrBadSignature
	}
	return nil
}
