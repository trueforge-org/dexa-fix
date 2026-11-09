package mainbackend

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/pkg/strengthlevel/client"
)

// Names and limits of the flows. The two-step verification flow is described in internal_twofactor.go,
// passkeys in internal_passkey.go, password reset in reset.go and TLS in tlsserve.go.
const (
	// providerWithings is the importer name of the Withings integration.
	providerWithings = "withings"

	oauthCookiePrefix = "oauth_state_"
	oauthStateTTL     = 10 * time.Minute
	maxFlashLen       = 300

	resetAdminTTL = 24 * time.Hour
	resetEmailTTL = time.Hour

	maxResetIP   = 10 // reset e-mails per client address within the failure window
	maxResetAddr = 3  // reset e-mails per e-mail address within the failure window
	resetPath    = "/password/reset"

	settingRequire2FA   = "auth.require_2fa"
	require2FAOff       = "off"
	require2FAAdmins    = "admins"
	require2FAEveryone  = "everyone"
	mfaCookie           = "dexafix_mfa"
	sealMFA             = "mfa"
	mfaTTL              = 5 * time.Minute
	maxMFAFail          = 5 // wrong codes per account within the failure window
	defaultTOTPIssuer   = "dexa-fix"
	secondFactorMessage = "That code is not right. Try the next code, or use a recovery code."

	passkeyCookie       = "dexafix_webauthn"
	sealPasskeyRegister = "webauthn-register"
	sealPasskeyLogin    = "webauthn-login"
	passkeyTTL          = 5 * time.Minute
	maxPasskeyName      = 64
	defaultPasskeyName  = "Passkey"

	maxTokenNameLen = 64
	maxEmailLen     = 254
	minHeightCm     = 50
	maxHeightCm     = 280

	// reauthWindow is how recent the sign-in of a user without a password must be for a sensitive
	// change, where recent sign-in is required.
	reauthWindow = 10 * time.Minute

	// scalePageSize is how many readings the scale page shows at a time.
	scalePageSize = 100

	settingTLSMode  = "tls.mode"
	settingTLSCert  = "tls.cert"  // PEM chain, stored encrypted with the other settings
	settingTLSKey   = "tls.key"   // PEM private key, stored encrypted
	settingTLSNames = "tls.names" // names for a self-signed certificate
	settingHTTP3    = "http3.enabled"
	settingHTTPS    = "tls.port" // TCP port for HTTPS; HTTP/3 uses the same number on UDP

	defaultHTTPSPort = "8443"

	selfSignedDays  = 365
	selfSignedRenew = 30 * 24 * time.Hour // regenerate when less than this is left
)

// label capitalises a provider name for display.
func label(name string) string {
	if name == "" {
		return name
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

func dayOrDash(t *time.Time, none string) string {
	if t == nil {
		return none
	}
	return t.UTC().Format(dateLayout)
}

func userPath(id int64, tab string) string {
	p := "/u/" + strconv.FormatInt(id, 10)
	if tab != "" {
		p += "/" + tab
	}
	return p
}

// settingsPath is the address of a user's settings page with a message to show.
func settingsPath(userID int64, msg string) string {
	if len(msg) > maxFlashLen {
		msg = msg[:maxFlashLen]
	}
	return userPath(userID, "settings") + "?msg=" + url.QueryEscape(msg)
}

// strengthLevelError turns a client error into a sentence for the user; details go to the log.
func strengthLevelError(err error) string {
	switch {
	case errors.Is(err, client.ErrLogin):
		return "the e-mail address or password was refused."
	case errors.Is(err, client.ErrPage):
		log.Warn().Err(err).Msg("strengthlevel page changed")
		return "Strength Level looks different than expected, so it may have changed."
	}
	log.Warn().Err(err).Msg("strengthlevel request failed")
	return "Strength Level could not be reached."
}

// flashParam reads the message passed to a page after a redirect.
func flashParam(r *http.Request) string {
	msg := r.URL.Query().Get("msg")
	if len(msg) > maxFlashLen {
		msg = msg[:maxFlashLen]
	}
	return msg
}
