package mainbackend

import (
	"strings"
	"testing"
	"time"
)

func TestASecretThatIsNotBase32GivesNoCodeAndAcceptsNone(t *testing.T) {
	if code, err := totpCode("not base32 !!", 1); err == nil || code != "" {
		t.Fatalf("code %q, err %v", code, err)
	}
	if _, ok := checkTOTP("not base32 !!", "123456", time.Now()); ok {
		t.Fatal("a code was accepted for a broken secret")
	}
}

func TestATextTooLongForAQRCodeIsRefused(t *testing.T) {
	if svg, err := qrSVG(strings.Repeat("x", 5000)); err == nil || svg != "" {
		t.Fatalf("svg of %d bytes, err %v", len(svg), err)
	}
}
