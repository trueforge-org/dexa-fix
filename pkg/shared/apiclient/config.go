package apiclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// The values of FrontendConfig.TLSMode.
const (
	TLSOff        = "off"
	TLSUpload     = "upload"
	TLSSelfSigned = "selfsigned"
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

	// Analytics are the optional measuring and ad services an administrator turned on. The frontend
	// loads them, after a visitor agrees, on the public pages only.
	Analytics Analytics `json:"analytics"`
}

// Analytics holds the identifiers of the optional services. An empty one is off.
type Analytics struct {
	ScarfPixelID      string `json:"scarf_pixel_id,omitempty"`      // a UUID
	GoogleAnalyticsID string `json:"google_analytics_id,omitempty"` // G-XXXXXXXXXX
	AdSensePublisher  string `json:"adsense_publisher,omitempty"`   // ca-pub-0000000000000000
}

// Any reports whether at least one service is on.
func (a Analytics) Any() bool {
	return a.ScarfPixelID != "" || a.GoogleAnalyticsID != "" || a.AdSensePublisher != ""
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
