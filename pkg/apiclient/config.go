package apiclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// FrontendConfig is what the frontend process needs from the administrator settings to serve
// visitors: who its trusted proxies are, its public address, and the TLS material for the HTTPS and
// HTTP/3 listeners it owns.
type FrontendConfig struct {
	// Generation changes with every change to the settings.
	Generation int64  `json:"generation"`
	Version    string `json:"version"`

	BaseURL        string   `json:"base_url,omitempty"`
	TrustedProxies []string `json:"trusted_proxies,omitempty"` // prefixes in CIDR form
	ProxyProtocol  bool     `json:"proxy_protocol"`

	TLSMode   string `json:"tls_mode"` // off, upload or selfsigned
	CertPEM   string `json:"cert_pem,omitempty"`
	KeyPEM    string `json:"key_pem,omitempty"` // only over the authenticated internal link
	HTTPSPort string `json:"https_port,omitempty"`
	HTTP3     bool   `json:"http3"`
}

// FrontendConfig fetches the configuration. If etag is the tag of the answer last seen and nothing
// changed, it returns (nil, etag, nil).
func (c *Client) FrontendConfig(ctx context.Context, etag string) (*FrontendConfig, string, error) {
	h := http.Header{}
	if etag != "" {
		h.Set("If-None-Match", etag)
	}
	resp, err := c.Raw(ctx, http.MethodGet, "/frontend-config", nil, h)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified:
		return nil, etag, nil
	case resp.StatusCode >= 400:
		return nil, "", errorFrom(resp)
	}
	var cfg FrontendConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, "", fmt.Errorf("backend answer: %w", err)
	}
	return &cfg, resp.Header.Get("ETag"), nil
}
