package web

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// rfcSecret is the RFC 6238 test secret, the ASCII string "12345678901234567890", in base32.
const rfcSecret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

func TestTOTPRFC6238Vectors(t *testing.T) {
	// RFC 6238 gives 8-digit codes; the apps use the last 6 digits of the same value.
	for _, tt := range []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	} {
		got, err := totpCode(rfcSecret, totpStep(time.Unix(tt.unix, 0)))
		if err != nil {
			t.Fatalf("time %d: %v", tt.unix, err)
		}
		if got != tt.want {
			t.Errorf("time %d: code %s, want %s", tt.unix, got, tt.want)
		}
	}
}

func TestCheckTOTPWindow(t *testing.T) {
	const n = int64(1000000) // the step under test
	code, err := totpCode(rfcSecret, n)
	if err != nil {
		t.Fatal(err)
	}
	at := func(step int64) time.Time { return time.Unix(step*totpPeriod+15, 0) }

	for _, step := range []int64{n - 1, n, n + 1} {
		got, ok := checkTOTP(rfcSecret, code, at(step))
		if !ok || got != n {
			t.Errorf("at step %d: got step %d ok %v, want step %d accepted", step, got, ok, n)
		}
	}
	for _, step := range []int64{n + 2, n - 2} {
		if got, ok := checkTOTP(rfcSecret, code, at(step)); ok {
			t.Errorf("at step %d: accepted as step %d, want rejected", step, got)
		}
	}

	// spaces in the input are ignored
	spaced := code[:3] + " " + code[3:]
	if got, ok := checkTOTP(rfcSecret, spaced, at(n)); !ok || got != n {
		t.Errorf("spaced code %q: got step %d ok %v", spaced, got, ok)
	}

	// wrong length or non-digits are rejected
	for _, bad := range []string{"", "12345", "1234567", "12345a", "abcdef", code + "0"} {
		if _, ok := checkTOTP(rfcSecret, bad, at(n)); ok {
			t.Errorf("input %q accepted", bad)
		}
	}
}

var recoveryCodeShape = regexp.MustCompile(`^[a-z2-7]{4}-[a-z2-7]{4}-[a-z2-7]{4}-[a-z2-7]{4}$`)

func TestRecoveryCodes(t *testing.T) {
	codes, err := newRecoveryCodes(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 10 {
		t.Fatalf("got %d codes, want 10", len(codes))
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if !recoveryCodeShape.MatchString(c) {
			t.Errorf("code %q does not have the xxxx-xxxx-xxxx-xxxx shape", c)
		}
		if seen[c] {
			t.Errorf("code %q is not distinct", c)
		}
		seen[c] = true
	}

	c := codes[0]
	want := recoveryHash(c)
	upper := strings.ToUpper(strings.ReplaceAll(c, "-", ""))
	spaced := strings.Join(strings.Split(c, "-"), " ")
	if got := recoveryHash(upper); got != want {
		t.Errorf("upper case without dashes hashes differently")
	}
	if got := recoveryHash(spaced); got != want {
		t.Errorf("spaces instead of dashes hash differently")
	}
	if recoveryHash(codes[1]) == want {
		t.Errorf("different codes hash the same")
	}
}

func TestLooksLikeTOTP(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want bool
	}{
		{"123456", true},
		{" 123 456 ", true},
		{"12345", false},
		{"1234567", false},
		{"12345a", false},
		{"", false},
		{"abcd-efgh-ijkl-mnop", false},
		{"abcdefghijklmnop", false},
	} {
		if got := looksLikeTOTP(tt.in); got != tt.want {
			t.Errorf("looksLikeTOTP(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestOtpauthURI(t *testing.T) {
	uri := otpauthURI("dexa-fix", "alice@example.com", rfcSecret)
	if !strings.HasPrefix(uri, "otpauth://totp/") {
		t.Errorf("uri %q does not start with otpauth://totp/", uri)
	}
	for _, want := range []string{"secret=" + rfcSecret, "issuer=dexa-fix"} {
		if !strings.Contains(uri, want) {
			t.Errorf("uri %q lacks %q", uri, want)
		}
	}
}

func TestQRSVG(t *testing.T) {
	svg, err := qrSVG(otpauthURI("dexa-fix", "alice@example.com", rfcSecret))
	if err != nil {
		t.Fatal(err)
	}
	s := string(svg)
	if !strings.HasPrefix(s, "<svg") {
		t.Errorf("output does not start with <svg: %.40s", s)
	}
	if !strings.Contains(s, "viewBox") {
		t.Error("output has no viewBox")
	}
	if strings.Contains(strings.ToLower(s), "<script") {
		t.Error("output contains a script")
	}
}
