package mainbackend

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
)

// frontendConfig describes the settings the frontend acts on. The private key is included because
// the frontend terminates TLS; the internal link is authenticated and meant for a private network.
func (s *Backend) frontendConfig(r *http.Request) (apiclient.FrontendConfig, error) {
	ctx := r.Context()
	gen, err := s.store.SettingsGeneration(ctx)
	if err != nil {
		return apiclient.FrontendConfig{}, err
	}
	l := s.cfg()
	cfg := apiclient.FrontendConfig{
		Generation:    gen,
		Version:       s.Version,
		ProxyProtocol: l.proxyProtocol,
		TLSMode:       l.tlsMode,
		HTTPSPort:     l.httpsPort,
		HTTP3:         l.http3,
		Analytics:     l.analytics,
	}
	if l.baseURL != nil {
		cfg.BaseURL = l.baseURL.String()
	}
	for _, p := range l.trusted {
		cfg.TrustedProxies = append(cfg.TrustedProxies, p.String())
	}
	if l.tlsCert != nil { // only a certificate that loaded is shipped
		if cfg.CertPEM, err = s.settingValue(ctx, settingTLSCert); err != nil {
			return cfg, err
		}
		if cfg.KeyPEM, err = s.settingValue(ctx, settingTLSKey); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

func (s *Backend) internalFrontendConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.frontendConfig(r)
	if err != nil {
		internalError(w, err)
		return
	}
	sum := sha256.New()
	enc := hashJSON(sum, cfg)
	etag := `"` + hex.EncodeToString(enc) + `"`
	w.Header().Set("ETag", etag)
	for _, t := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		if strings.TrimSpace(t) == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	internalJSON(w, http.StatusOK, cfg)
}

func hashJSON(h interface {
	Write([]byte) (int, error)
	Sum([]byte) []byte
}, v any) []byte {
	b, _ := json.Marshal(v)
	h.Write(b)
	return h.Sum(nil)
}
