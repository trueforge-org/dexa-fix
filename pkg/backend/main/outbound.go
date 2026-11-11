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

// allowPrivate is asked at each outgoing connection, so the setting applies without a restart.
func (s *Backend) allowPrivate() bool { return s.cfg().allowPrivate }

// newProviderClient is the HTTP client for the sign-in providers: the default transport (proxy
// from the environment, HTTP/2) with the guard on its dialer, and redirects held to the same rule as
// the address an administrator typed.
func (s *Backend) newProviderClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: netguard.Control(s.allowPrivate)}).DialContext
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
// that has a remedy.
func outboundFailure(err error, base string) string {
	log.Warn().Err(err).Str("action", base).Msg("outgoing connection failed")
	if errors.Is(err, netguard.ErrBlocked) {
		return base + ": that address is on a private or link-local network. If the server really is on your own network, turn on \"Allow sign-in providers and the mail server on private network addresses\" under Network and retention."
	}
	return base + ": the server could not be reached or did not answer as expected. Check the address, the port and the encryption; the details are in the server log."
}
