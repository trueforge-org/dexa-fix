package mainbackend

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 authenticator apps all use HMAC-SHA1
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"html/template"
	"net/url"
	"strings"
	"time"

	"rsc.io/qr"
)

// Time-based one-time passwords (RFC 6238) as authenticator apps compute them: HMAC-SHA1 over the
// 30-second step counter, 6 digits. Nothing here is configurable, because the apps ignore most
// settings in the otpauth link anyway.
const (
	totpPeriod = 30 // seconds per step
	totpDigits = 6
	totpSkew   = 1 // steps accepted before and after the current one, for clock drift

	recoveryCodeCount = 10
)

var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// newTOTPSecret returns a random 160-bit shared secret in the base32 form apps expect.
func newTOTPSecret() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b) // never fails: since Go 1.24 a broken random source ends the program
	return base32NoPad.EncodeToString(b)
}

// totpCode computes the code for one step.
func totpCode(secret string, step int64) (string, error) {
	key, err := base32NoPad.DecodeString(strings.ToUpper(strings.ReplaceAll(secret, " ", "")))
	if err != nil {
		return "", err
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	n := (binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff) % 1_000_000
	return fmt.Sprintf("%0*d", totpDigits, n), nil
}

// totpStep is the step a moment falls in.
func totpStep(t time.Time) int64 { return t.Unix() / totpPeriod }

// checkTOTP verifies a code typed by the user against the steps around now. It returns the step
// that matched, which the caller records so the same code cannot be used again. Spaces in the
// input are ignored. Every candidate is compared, in constant time.
func checkTOTP(secret, input string, now time.Time) (step int64, ok bool) {
	input = strings.ReplaceAll(strings.TrimSpace(input), " ", "")
	if len(input) != totpDigits {
		return 0, false
	}
	cur := totpStep(now)
	for d := int64(-totpSkew); d <= totpSkew; d++ {
		want, err := totpCode(secret, cur+d)
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(input)) == 1 && !ok {
			step, ok = cur+d, true
		}
	}
	return step, ok
}

// otpauthURI is the link an authenticator app reads from the QR code.
func otpauthURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{"secret": {secret}, "issuer": {issuer}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// qrSVG draws text as a QR code in an inline SVG. It is generated here from a fixed template, so
// it is safe as trusted HTML, and being inline it needs no script, image source or inline style.
func qrSVG(text string) (template.HTML, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	const quiet = 4
	size := code.Size + 2*quiet
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="220" height="220" role="img" aria-label="QR code for your authenticator app" shape-rendering="crispEdges"><rect width="100%%" height="100%%" fill="#fff"/><path fill="#000" d="`, size, size)
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; x++ {
			if code.Black(x, y) {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return template.HTML(b.String()), nil //nolint:gosec // built above from numbers only
}

// newRecoveryCodes returns count fresh codes as shown to the user (xxxx-xxxx-xxxx-xxxx, 80 bits).
func newRecoveryCodes(count int) []string {
	out := make([]string, 0, count)
	for range count {
		b := make([]byte, 10)
		_, _ = rand.Read(b)
		s := strings.ToLower(base32NoPad.EncodeToString(b))
		out = append(out, s[0:4]+"-"+s[4:8]+"-"+s[8:12]+"-"+s[12:16])
	}
	return out
}

// recoveryHash is what is stored for a recovery code. The codes are long random values, so a fast
// hash is enough, like for session tokens. Case, dashes and spaces in the input do not matter.
func recoveryHash(code string) string {
	code = strings.ToLower(code)
	code = strings.NewReplacer("-", "", " ", "").Replace(code)
	return hashToken("recovery:" + code)
}

// looksLikeTOTP tells a 6-digit code from a recovery code in a single input field.
func looksLikeTOTP(input string) bool {
	input = strings.ReplaceAll(strings.TrimSpace(input), " ", "")
	if len(input) != totpDigits {
		return false
	}
	for _, c := range input {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
