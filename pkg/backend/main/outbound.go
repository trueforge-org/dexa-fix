package mainbackend

import (
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexafix/pkg/shared/netguard"
)

// The backend makes two kinds of outgoing connection on its own: to the sign-in providers an
// administrator configured, and to the mail server. Both addresses are typed into the Admin page, so
// both go through the guard in pkg/shared/netguard, which refuses cloud metadata and other link-local
// addresses always and private networks unless the administrator allowed them. (Everything else that
// talks to the outside is a worker's, which has no database and no keys.)

const maxProviderRedirects = 5

// The two settings are asked at each outgoing connection, so they apply without a restart. They are
// separate because they are separate risks: a provider is an address an administrator types once and
// the server then talks to on behalf of every visitor, the mail server is one they type for e-mail.
func (s *Backend) allowPrivateProviders() bool { return s.cfg().allowPrivateProviders }
func (s *Backend) allowPrivateMail() bool      { return s.cfg().allowPrivateMail }

// The names of the settings, as the Admin page shows them, for the messages that point to them.
const (
	settingLabelProviders = "Allow sign-in providers on private network addresses"
	settingLabelMail      = "Allow the mail server on a private network address"
)

// newProviderClient is the HTTP client for the sign-in providers: the default transport (proxy
// from the environment, HTTP/2) with the guard on its dialer, and redirects held to the same rule as
// the address an administrator typed.
func (s *Backend) newProviderClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: netguard.Control(s.allowPrivateProviders)}).DialContext
	return &http.Client{Timeout: providerTimeout, Transport: t, CheckRedirect: checkProviderRedirect}
}

func checkProviderRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxProviderRedirects {
		return errors.New("too many redirects")
	}
	if !validProviderURL(req.URL.String()) {
		return errors.New("redirected to an address that is not allowed")
	}
	return nil
}

// outboundFailure is what an administrator is told when a connection to a provider or the mail
// server failed: base ("Could not send the test e-mail"), a colon and the reason. The error itself
// can hold what the other end answered (a page of an internal service, say), so it goes to the log
// and not to the page; the one thing worth saying is that the guard refused the address, because
// that has a remedy: the setting named by settingLabel.
func outboundFailure(err error, base, settingLabel string) string {
	log.Warn().Err(err).Str("action", base).Msg("outgoing connection failed")
	if errors.Is(err, netguard.ErrBlocked) {
		return base + ": that address is on a private or link-local network. If the server really is on your own network, turn on \"" + settingLabel + "\" under Network and retention."
	}
	return base + ": the server could not be reached or did not answer as expected. Check the address, the port and the encryption; the details are in the server log."
}
