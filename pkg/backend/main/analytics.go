package mainbackend

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
)

// The optional analytics and ad services (Scarf, Google Analytics, Google AdSense). They are off
// until an administrator enters an identifier on the Admin page, and the frontend loads them only on
// the pages a visitor sees before signing in, and only after the visitor agrees (docs, and the
// privacy policy that says so). The backend stores and validates the identifiers; it never talks to
// the services.

var (
	scarfPixelPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	googleAnalytics   = regexp.MustCompile(`^G-[A-Z0-9]{4,20}$`)
	adSensePublisher  = regexp.MustCompile(`^ca-pub-[0-9]{10,20}$`)
)

// validateAnalytics trims the identifiers and checks their shape. They end up in a page and in a
// content security policy, so nothing else gets through. The message is for the administrator.
func validateAnalytics(in apiclient.Analytics) (apiclient.Analytics, string) {
	out := apiclient.Analytics{
		ScarfPixelID:      strings.TrimSpace(in.ScarfPixelID),
		GoogleAnalyticsID: strings.TrimSpace(in.GoogleAnalyticsID),
		AdSensePublisher:  strings.TrimSpace(in.AdSensePublisher),
	}
	switch {
	case out.ScarfPixelID != "" && !scarfPixelPattern.MatchString(out.ScarfPixelID):
		return out, "The Scarf pixel ID is a UUID such as 123e4567-e89b-12d3-a456-426614174000."
	case out.GoogleAnalyticsID != "" && !googleAnalytics.MatchString(out.GoogleAnalyticsID):
		return out, "The Google Analytics measurement ID looks like G-ABC123XYZ9."
	case out.AdSensePublisher != "" && !adSensePublisher.MatchString(out.AdSensePublisher):
		return out, "The AdSense publisher ID looks like ca-pub-1234567890123456."
	}
	return out, ""
}

// analyticsSettings reads the stored identifiers. One that is no longer valid is ignored, so a
// hand-edited database cannot put markup in a page.
func (s *Backend) analyticsSettings(ctx context.Context) (apiclient.Analytics, error) {
	var in apiclient.Analytics
	for _, f := range []struct {
		name string
		to   *string
	}{{settingScarfPixel, &in.ScarfPixelID}, {settingGoogleAnalytics, &in.GoogleAnalyticsID}, {settingAdSense, &in.AdSensePublisher}} {
		v, err := s.settingValue(ctx, f.name)
		if err != nil {
			return apiclient.Analytics{}, err
		}
		*f.to = v
	}
	out, msg := validateAnalytics(in)
	if msg != "" {
		return apiclient.Analytics{}, nil
	}
	return out, nil
}

func (s *Backend) internalAdminSaveAnalytics(w http.ResponseWriter, r *http.Request, _ models.User) {
	var in apiclient.Analytics
	if !internalDecode(w, r, &in) {
		return
	}
	a, msg := validateAnalytics(in)
	if msg != "" {
		adminMsg(w, msg)
		return
	}
	ctx := r.Context()
	for _, p := range []struct{ name, value string }{
		{settingScarfPixel, a.ScarfPixelID}, {settingGoogleAnalytics, a.GoogleAnalyticsID}, {settingAdSense, a.AdSensePublisher},
	} {
		if err := s.putOrClear(ctx, p.name, p.value); err != nil {
			internalError(w, err)
			return
		}
	}
	if err := s.reloadSettings(ctx); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, 0, auditSettingChanged, "analytics_settings")
	if a.Any() {
		adminMsgChanged(w, "Analytics and ads saved. Visitors who are not signed in are asked before anything is loaded.")
		return
	}
	adminMsgChanged(w, "Analytics and ads are off.")
}
