package apiclient

import (
	"context"

	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// SessionCookie is the name of the cookie that holds the session. The browser sends it to the
// frontend, which passes it on to the backend (HeaderCookie).
const SessionCookie = "dexafix_session"

// SessionUser is the signed-in user as the frontend needs it. models.User leaves some fields out
// of its JSON form on purpose (they are not part of the public API), so the internal API carries
// them here.
type SessionUser struct {
	models.User
	HiddenColumns     []string `json:"hidden_columns"`
	StrengthLevelAuto bool     `json:"strength_level_auto"`
	StrengthLevelBulk bool     `json:"strength_level_bulk"`
	DataVersion       int64    `json:"data_version"`
}

// NewSessionUser copies a user for the wire.
func NewSessionUser(u models.User) SessionUser {
	return SessionUser{User: u, HiddenColumns: u.HiddenColumns, StrengthLevelAuto: u.StrengthLevelAuto,
		StrengthLevelBulk: u.StrengthLevelBulk, DataVersion: u.DataVersion}
}

// Model returns the user with every field filled in again.
func (s SessionUser) Model() models.User {
	u := s.User
	u.HiddenColumns, u.StrengthLevelAuto, u.StrengthLevelBulk, u.DataVersion =
		s.HiddenColumns, s.StrengthLevelAuto, s.StrengthLevelBulk, s.DataVersion
	return u
}

// Session returns the user behind the visitor's session cookie, or nil if nobody is signed in.
func (c *Client) Session(ctx context.Context) (*models.User, error) {
	var su SessionUser
	err := c.Get(ctx, "/session", &su)
	if IsCode(err, CodeUnauthorized) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u := su.Model()
	return &u, nil
}
