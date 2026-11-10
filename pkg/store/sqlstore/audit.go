package sqlstore

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/store"
)

type auditRow struct {
	bun.BaseModel `bun:"table:audit_log"`

	ID        int64     `bun:"id,pk,autoincrement"`
	CreatedAt time.Time `bun:"created_at"`
	ActorID   *int64    `bun:"actor_id"`
	TargetID  *int64    `bun:"target_id"`
	Action    string    `bun:"action"`
	Detail    string    `bun:"detail"`
	IP        string    `bun:"ip"`
}

const (
	defaultAuditLimit = 100
	maxAuditLimit     = 100000
	maxAuditField     = 500
)

func clip(s string) string {
	if len(s) > maxAuditField {
		return s[:maxAuditField]
	}
	return s
}

func (s *Store) AddAuditEvent(ctx context.Context, e *models.AuditEvent) error {
	if e.Action == "" {
		return store.ErrInvalid
	}
	e.At = now()
	row := auditRow{CreatedAt: e.At, ActorID: e.ActorID, TargetID: e.TargetID, Action: clip(e.Action), Detail: clip(e.Detail), IP: clip(e.IP)}
	if _, err := s.db.NewInsert().Model(&row).Exec(ctx); err != nil {
		if isFK(err) { // a user deleted a moment ago: keep the event without them
			row.ID, row.ActorID, row.TargetID = 0, nil, nil
			_, err = s.db.NewInsert().Model(&row).Exec(ctx)
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}
	e.ID = row.ID
	return nil
}

func (s *Store) ListAuditEvents(ctx context.Context, f store.AuditFilter) ([]models.AuditEvent, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultAuditLimit
	}
	limit = min(limit, maxAuditLimit)
	var rows []auditRow
	q := s.db.NewSelect().Model(&rows).OrderExpr("id DESC").Limit(int64(limit))
	if f.UserID != 0 {
		q = q.Where("(actor_id = ? OR target_id = ?)", f.UserID, f.UserID)
	}
	if f.BeforeID != 0 {
		q = q.Where("id < ?", f.BeforeID)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]models.AuditEvent, 0, len(rows))
	for _, r := range rows {
		out = append(out, models.AuditEvent{ID: r.ID, At: r.CreatedAt, ActorID: r.ActorID, TargetID: r.TargetID, Action: r.Action, Detail: r.Detail, IP: r.IP})
	}
	return out, nil
}

func (s *Store) PurgeAuditEvents(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.NewDelete().Model((*auditRow)(nil)).Where("created_at < ?", before.UTC()).Exec(ctx)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
