package mainbackend

import (
	"context"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
)

// sessionStore is where the sessions live: the database (store.Store) or, with WithState, Valkey
// (valkeySessions). The two are never mixed. Everything that starts or ends a session goes through
// it, so a session cannot outlive the thing that should have ended it:
//
//   - sign-out ends one (internalLogout);
//   - a changed or reset password ends every session of the user, and so does an administrator
//     setting one (internalPasswordChange, internalAdminSetPassword, the reset step). These run
//     before the password is written, and fail the request when they fail, so a password is never
//     changed while the old sessions could still be used;
//   - deleting an account ends them too (internalAdminDeleteUser). Even if that call fails the
//     session is dead, because the user row is read on every request and no longer exists;
//   - a session expires by itself (sessionTTL).
type sessionStore interface {
	CreateSession(ctx context.Context, tokenHash string, userID int64, method string, expires time.Time) error
	// UserBySession returns the user of an unexpired session, or store.ErrNotFound.
	UserBySession(ctx context.Context, tokenHash string) (models.User, error)
	GetSession(ctx context.Context, tokenHash string) (models.Session, error)
	DeleteSession(ctx context.Context, tokenHash string) error
	DeleteUserSessions(ctx context.Context, userID int64) error
}

// valkeySessions keeps the sessions in Valkey. The user is not a copy: UserBySession asks the
// database for the user row of the session, so a changed role, a deleted account or a renamed user
// is seen at once, as with the sessions table.
type valkeySessions struct {
	SessionState
	users interface {
		GetUser(ctx context.Context, id int64) (models.User, error)
	}
}

func (v valkeySessions) UserBySession(ctx context.Context, tokenHash string) (models.User, error) {
	sess, err := v.GetSession(ctx, tokenHash)
	if err != nil {
		return models.User{}, err
	}
	return v.users.GetUser(ctx, sess.UserID)
}

// liveSessions is the number of sessions for the Admin page: Valkey's when it holds them, else the
// database's (fromDB).
func (s *Backend) liveSessions(ctx context.Context, fromDB int) int {
	if v, ok := s.sessions.(valkeySessions); ok {
		if n, err := v.CountSessions(ctx); err == nil {
			return int(n)
		}
	}
	return fromDB
}
