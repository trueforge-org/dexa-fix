package backend

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// resetURL builds the link for a token. With emailed set it uses the configured base URL only: an
// e-mailed link must never be built from the Host header of the request that asked for it.
func (s *Backend) resetURL(r *http.Request, token string, emailed bool) (link string, fromRequest bool) {
	q := "?t=" + url.QueryEscape(token)
	if base := s.cfg().baseURL; base != nil {
		return base.Scheme + "://" + base.Host + resetPath + q, false
	}
	if emailed {
		return "", true
	}
	return externalURL(r, resetPath) + q, true
}

func (s *Backend) newResetToken(ctx context.Context, userID int64, createdBy *int64, ttl time.Duration) (string, time.Time, error) {
	token, err := randomHex(32)
	if err != nil {
		return "", time.Time{}, err
	}
	expires := time.Now().Add(ttl)
	return token, expires, s.store.CreatePasswordReset(ctx, hashToken(token), userID, createdBy, expires)
}

func resetMail(name, link string, ttl time.Duration) (subject, body string) {
	return "Reset your dexa-fix password",
		"Hello " + name + ",\n\n" +
			"Someone asked to reset the password of your dexa-fix account. To choose a new password, open this link within " +
			ttl.String() + ":\n\n" + link + "\n\n" +
			"If you did not ask for this, ignore this e-mail: your password stays as it is.\n"
}

// sendResetMail sends the mail now and reports whether it went out.
func (s *Backend) sendResetMail(u models.User, link string, ttl time.Duration) bool {
	m := s.cfg().mailer
	if m == nil || u.Email == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(s.bg.Ctx, 40*time.Second)
	defer cancel()
	subject, body := resetMail(u.Name, link, ttl)
	if err := m.Send(ctx, u.Email, subject, body); err != nil {
		log.Warn().Err(err).Int64("user", u.ID).Msg("could not send the password reset e-mail")
		return false
	}
	return true
}

// forgetResets drops outstanding links after the password or e-mail address changed by other means.
func (s *Backend) forgetResets(ctx context.Context, userID int64) {
	if err := s.store.DeletePasswordResets(ctx, userID); err != nil {
		log.Error().Err(err).Int64("user", userID).Msg("could not remove password reset links")
	}
}
