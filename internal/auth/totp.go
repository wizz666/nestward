package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 TOTP uses HMAC-SHA1; authenticator apps expect it
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"

	"rsc.io/qr"
)

const totpStep = 30 // seconds

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a random secret, base32 encoded.
func NewTOTPSecret() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return b32.EncodeToString(b), nil
}

// totpAt returns the 6-digit code for one time step.
func totpAt(secret string, step int64) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.ReplaceAll(secret, " ", "")))
	if err != nil {
		return "", err
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	n := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", n%1_000_000), nil
}

// Code returns the current 6-digit code for a secret (what the
// authenticator app shows). Used by tests.
func Code(secret string, t time.Time) (string, error) { return totpAt(secret, Step(t)) }

// Step returns the TOTP time step for t.
func Step(t time.Time) int64 { return t.Unix() / totpStep }

// VerifyTOTP checks a code against the current step and one step either
// side (clock drift). It returns the matching step, which must be newer
// than lastStep so the same code cannot be used twice.
func VerifyTOTP(secret, code string, now time.Time, lastStep int64) (int64, bool) {
	code = strings.TrimSpace(strings.ReplaceAll(code, " ", ""))
	if len(code) != 6 {
		return 0, false
	}
	cur := Step(now)
	for _, s := range []int64{cur - 1, cur, cur + 1} {
		if s <= lastStep {
			continue
		}
		want, err := totpAt(secret, s)
		if err != nil {
			return 0, false
		}
		if hmac.Equal([]byte(want), []byte(code)) {
			return s, true
		}
	}
	return 0, false
}

// OTPAuthURI is the link authenticator apps read from the QR code.
func OTPAuthURI(secret, username string) string {
	label := url.PathEscape("Nestward:" + username)
	v := url.Values{"secret": {secret}, "issuer": {"Nestward"}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return "otpauth://totp/" + label + "?" + v.Encode()
}

// QRDataURI renders text as a PNG QR code in a data: address.
func QRDataURI(text string) (string, error) {
	c, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	c.Scale = 6
	var buf bytes.Buffer
	buf.Write(c.PNG())
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}
