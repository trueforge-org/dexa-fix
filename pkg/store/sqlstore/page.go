package sqlstore

import (
	"context"

	"github.com/uptrace/bun"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// window applies a ListFilter to a query on a table with user_id, measured_at and id columns. It
// asks for one row more than the limit, which tells whether another page exists.
func window(q *bun.SelectQuery, userID int64, f store.ListFilter) *bun.SelectQuery {
	q = q.Where("user_id = ?", userID)
	if !f.Since.IsZero() {
		q = q.Where("measured_at >= ?", f.Since.UTC())
	}
	if !f.Until.IsZero() {
		q = q.Where("measured_at <= ?", f.Until.UTC())
	}
	if f.After != nil {
		q = q.Where("(measured_at < ? OR (measured_at = ? AND id < ?))", f.After.MeasuredAt.UTC(), f.After.MeasuredAt.UTC(), f.After.ID)
	}
	q = q.OrderExpr("measured_at DESC, id DESC")
	if f.Limit > 0 {
		q = q.Limit(int64(f.Limit) + 1)
	}
	return q
}

func (s *Store) ListDexaScansPage(ctx context.Context, userID int64, f store.ListFilter) ([]models.DexaScan, *store.Cursor, error) {
	var rows []dexaRow
	if err := window(s.db.NewSelect().Model(&rows), userID, f).Scan(ctx); err != nil {
		return nil, nil, err
	}
	var next *store.Cursor
	if f.Limit > 0 && len(rows) > f.Limit {
		rows = rows[:f.Limit]
		last := rows[len(rows)-1]
		next = &store.Cursor{MeasuredAt: last.MeasuredAt.UTC(), ID: last.ID}
	}
	out, err := s.dexaModels(rows)
	if err != nil {
		return nil, nil, err
	}
	if out == nil {
		out = []models.DexaScan{}
	}
	return out, next, nil
}

func (s *Store) ListScaleReadingsPage(ctx context.Context, userID int64, f store.ListFilter) ([]models.ScaleReading, *store.Cursor, error) {
	var rows []scaleRow
	if err := window(s.db.NewSelect().Model(&rows), userID, f).Scan(ctx); err != nil {
		return nil, nil, err
	}
	var next *store.Cursor
	if f.Limit > 0 && len(rows) > f.Limit {
		rows = rows[:f.Limit]
		last := rows[len(rows)-1]
		next = &store.Cursor{MeasuredAt: last.MeasuredAt.UTC(), ID: last.ID}
	}
	out, err := s.scaleModels(rows)
	if err != nil {
		return nil, nil, err
	}
	if out == nil {
		out = []models.ScaleReading{}
	}
	return out, next, nil
}
