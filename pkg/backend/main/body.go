package mainbackend

import (
	"context"
	"errors"
	"fmt"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/pkg/estimate"
	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/shared/bodyapi"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// The scale readings, the DEXA scans and the estimates are held by the body service, not by this
// backend: it reaches them through a bodyapi.Client, with a short-lived token for one user and one
// scope on every call. This backend authenticates people and decides who may see what; the body
// service only trusts the token. Nothing here reads the scans or readings from the store.

// WithBody gives the backend the body service to call. A backend cannot be built without one.
func WithBody(c *bodyapi.Client) Option { return func(s *Backend) { s.body = bodyService{c: c} } }

// bodyService wraps the client: it builds the subject of each call from the user's row and turns
// the service's errors into the ones the handlers already understand.
type bodyService struct{ c *bodyapi.Client }

func (b bodyService) subject(u models.User) bodyapi.Subject {
	return bodyapi.Subject{ID: u.ID, Params: bodyapi.EstimateParams{
		Timezone: u.Timezone, Ranking: u.MethodRanking, AthleteDefault: u.AthleteDefault, HeightCm: u.HeightCm,
	}}
}

// bodyInvalid is a refusal by the body service. It reads like the store's own ErrInvalid errors
// (same text) and matches them with errors.Is.
type bodyInvalid struct{ msg string }

func (e bodyInvalid) Error() string        { return e.msg }
func (e bodyInvalid) Is(target error) bool { return target == store.ErrInvalid }

// bodyErr maps an error from the body service onto the errors the handlers map to answers. An
// unreachable or failing service stays a bodyapi.ErrUnavailable; a refusal of the service's own
// credentials is a bug or a misconfiguration, so it is logged and answered as an internal error.
func bodyErr(err error) error {
	var be *bodyapi.Error
	if err == nil || !errors.As(err, &be) {
		return err
	}
	switch be.Code {
	case bodyapi.CodeNotFound:
		return store.ErrNotFound
	case bodyapi.CodeInvalid:
		return bodyInvalid{be.Message}
	case bodyapi.CodeUnavailable:
		return err
	}
	log.Error().Str("code", be.Code).Str("message", be.Message).Msg("the body service refused a call")
	return fmt.Errorf("body service: %s", be.Message)
}

func (b bodyService) Scans(ctx context.Context, u models.User, p bodyapi.Page) ([]models.DexaScan, string, error) {
	s, next, err := b.c.Scans(ctx, b.subject(u), p)
	return s, next, bodyErr(err)
}

func (b bodyService) CreateScan(ctx context.Context, u models.User, s *models.DexaScan) error {
	return bodyErr(b.c.CreateScan(ctx, b.subject(u), s))
}

func (b bodyService) DeleteScan(ctx context.Context, u models.User, id int64) error {
	return bodyErr(b.c.DeleteScan(ctx, b.subject(u), id))
}

func (b bodyService) Readings(ctx context.Context, u models.User, p bodyapi.Page) ([]models.ScaleReading, string, error) {
	r, next, err := b.c.Readings(ctx, b.subject(u), p)
	return r, next, bodyErr(err)
}

func (b bodyService) UpsertReadings(ctx context.Context, u models.User, rs []models.ScaleReading) (int, error) {
	n, err := b.c.UpsertReadings(ctx, b.subject(u), rs)
	return n, bodyErr(err)
}

func (b bodyService) DeleteReading(ctx context.Context, u models.User, id int64) error {
	return bodyErr(b.c.DeleteReading(ctx, b.subject(u), id))
}

func (b bodyService) Reconcile(ctx context.Context, u models.User, req bodyapi.ReconcileRequest) (bodyapi.ImportResult, error) {
	r, err := b.c.Reconcile(ctx, b.subject(u), req)
	return r, bodyErr(err)
}

func (b bodyService) Estimates(ctx context.Context, u models.User, ifNoneMatch string) (bodyapi.EstimatesReply, error) {
	r, err := b.c.Estimates(ctx, b.subject(u), ifNoneMatch)
	return r, bodyErr(err)
}

func (b bodyService) Export(ctx context.Context, u models.User) (bodyapi.Export, error) {
	e, err := b.c.Export(ctx, b.subject(u))
	return e, bodyErr(err)
}

func (b bodyService) Stats(ctx context.Context) (bodyapi.Stats, error) {
	s, err := b.c.Stats(ctx)
	return s, bodyErr(err)
}

// computeEstimates asks the body service for a user's estimates. The result is shared with the
// body service's cache, so callers must not modify it.
func (s *Backend) computeEstimates(ctx context.Context, u models.User) (estimate.Result, error) {
	r, err := s.body.Estimates(ctx, u, "")
	return r.Result, err
}

// deleteBodyData removes everything the body service holds for a user who was just deleted. If the
// service cannot be reached the data stays until the maintenance sweep finds it (sweepBodyOrphans),
// so a failure here is only logged: the person is gone either way.
func (s *Backend) deleteBodyData(ctx context.Context, userID int64) {
	if err := s.body.c.DeleteUser(ctx, userID); err != nil {
		log.Warn().Err(err).Int64("user", userID).Msg("could not remove a deleted user's readings and scans yet; the maintenance sweep will")
	}
}

// sweepBodyOrphans removes the body data of users that no longer exist. Ids are never reused, so
// an id that has no user is gone for good. The body service is asked first and the users after, so
// a user created in between cannot look like an orphan.
func (s *Backend) sweepBodyOrphans(ctx context.Context) error {
	withData, err := s.body.c.UserIDs(ctx)
	if err != nil || len(withData) == 0 {
		return err
	}
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return err
	}
	known := make(map[int64]bool, len(users))
	for _, u := range users {
		known[u.ID] = true
	}
	for _, id := range withData {
		if known[id] {
			continue
		}
		if err := s.body.c.DeleteUser(ctx, id); err != nil {
			return err
		}
		log.Info().Int64("user", id).Msg("removed the readings and scans of a deleted user")
	}
	return nil
}
