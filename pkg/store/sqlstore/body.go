package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// ---- body: dexa scans and scale readings ----

func (s *Store) CreateDexaScan(ctx context.Context, d *models.DexaScan) error {
	d.MeasuredAt = d.MeasuredAt.UTC().Truncate(24 * time.Hour)
	d.CreatedAt = now()
	row := dexaRow{
		UserID: d.UserID, MeasuredAt: d.MeasuredAt,
		WeightKg: d.WeightKg, FatRatioPct: d.FatRatioPct, FatMassKg: d.FatMassKg, FatFreeMassKg: d.FatFreeMassKg,
		LeanMassKg: d.LeanMassKg, BoneMassKg: d.BoneMassKg, VisceralFatMassKg: d.VisceralFatMassKg,
		BMDGCm2: d.BMDGCm2, TScore: d.TScore, ZScore: d.ZScore, AndroidFatPct: d.AndroidFatPct, GynoidFatPct: d.GynoidFatPct,
		Notes: d.Notes, CreatedAt: d.CreatedAt,
	}
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return err
		}
		return bumpBodyVersion(ctx, tx, d.UserID)
	})
	if err != nil {
		if isFK(err) {
			return store.ErrNotFound
		}
		return err
	}
	d.ID = row.ID
	return nil
}

// bumpBodyVersion records that a user's scans or readings have changed. It runs in the same
// transaction as the change, so a reader never sees new data under an old version or the other
// way round. The counter lives in the database, so every process sees it, whether the change came
// from the web app, the API, an import or a command. It is the body domain's own: the users table
// is not touched, so this domain can later leave the database of the main backend.
func bumpBodyVersion(ctx context.Context, db bun.IDB, userID int64) error {
	_, err := db.ExecContext(ctx, `INSERT INTO body_user_state (user_id, data_version) VALUES (?, 1)
		ON CONFLICT (user_id) DO UPDATE SET data_version = body_user_state.data_version + 1`, userID)
	return err
}

// BodyVersion returns the number of changes to the user's scans and readings so far.
func (s *Store) BodyVersion(ctx context.Context, userID int64) (int64, error) {
	var row bodyStateRow
	err := s.db.NewSelect().Model(&row).Where("user_id = ?", userID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return row.DataVersion, err
}

// DeleteUserBodyData removes the user's scans, readings and counter.
func (s *Store) DeleteUserBodyData(ctx context.Context, userID int64) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		for _, m := range []any{(*dexaRow)(nil), (*scaleRow)(nil), (*bodyStateRow)(nil)} {
			if _, err := tx.NewDelete().Model(m).Where("user_id = ?", userID).Exec(ctx); err != nil {
				return err
			}
		}
		return nil
	})
}

// BodyUserIDs lists the users that have a scan, a reading or a counter.
func (s *Store) BodyUserIDs(ctx context.Context) ([]int64, error) {
	var ids []int64
	err := s.db.NewRaw(`SELECT user_id FROM dexa_scans UNION SELECT user_id FROM scale_readings
		UNION SELECT user_id FROM body_user_state ORDER BY user_id`).Scan(ctx, &ids)
	return ids, err
}

// BodyStats counts the scans and readings.
func (s *Store) BodyStats(ctx context.Context) (store.BodyStats, error) {
	var st store.BodyStats
	n, err := s.db.NewSelect().Model((*dexaRow)(nil)).Count(ctx)
	if err != nil {
		return st, err
	}
	st.DexaScans = int(n)
	if n, err = s.db.NewSelect().Model((*scaleRow)(nil)).Count(ctx); err != nil {
		return st, err
	}
	st.ScaleReadings = int(n)
	return st, nil
}

func (s *Store) ListDexaScans(ctx context.Context, userID int64) ([]models.DexaScan, error) {
	var rows []dexaRow
	if err := s.db.NewSelect().Model(&rows).Where("user_id = ?", userID).
		OrderExpr("measured_at DESC, id DESC").Scan(ctx); err != nil {
		return nil, err
	}
	var out []models.DexaScan
	for i := range rows {
		out = append(out, rows[i].toModel())
	}
	return out, nil
}

func (s *Store) DeleteDexaScan(ctx context.Context, userID, id int64) error {
	return s.deleteOwned(ctx, (*dexaRow)(nil), userID, id)
}

// ---- scale readings ----

// upsertChunk is how many readings go into one INSERT. 500 rows of 16 columns stay far below the
// parameter limits of both SQLite and Postgres.
const upsertChunk = 500

func (s *Store) UpsertScaleReadings(ctx context.Context, userID int64, rs []models.ScaleReading) (int, error) {
	// Check everything first, so a bad reading late in a large batch fails before any work is done.
	rows := make([]scaleRow, 0, len(rs))
	for i := range rs {
		r := &rs[i]
		if r.MeasuredAt.IsZero() {
			return 0, fmt.Errorf("%w: measured_at is required", store.ErrInvalid)
		}
		if r.Source == "" {
			r.Source = models.SourceManual
		}
		if r.Method == "" {
			r.Method = models.MethodScale
		}
		if !models.ValidMethod(r.Method) {
			return 0, fmt.Errorf("%w: unknown method %q", store.ErrInvalid, r.Method)
		}
		rows = append(rows, scaleRow{
			UserID: userID, MeasuredAt: r.MeasuredAt.UTC(),
			WeightKg: r.WeightKg, FatRatioPct: r.FatRatioPct, FatMassKg: r.FatMassKg, FatFreeMassKg: r.FatFreeMassKg,
			MuscleMassKg: r.MuscleMassKg, HydrationKg: r.HydrationKg, BoneMassKg: r.BoneMassKg, VisceralFatIndex: r.VisceralFatIndex,
			Source: r.Source, Method: r.Method, AthleteMode: r.AthleteMode, ExternalID: r.ExternalID,
			Notes: r.Notes, CreatedAt: now(),
		})
	}
	rows = lastPerExternalID(rows)

	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// A re-delivered reading without notes (every Withings import) must not wipe the note
		// the user added in the app, so those rows are upserted without refreshing notes.
		var withNotes, withoutNotes []scaleRow
		for _, r := range rows {
			if r.Notes == "" {
				withoutNotes = append(withoutNotes, r)
			} else {
				withNotes = append(withNotes, r)
			}
		}
		for _, part := range []struct {
			rows []scaleRow
			cols []string
		}{{withNotes, scaleValueCols}, {withoutNotes, scaleValueColsKeepNotes}} {
			for start := 0; start < len(part.rows); start += upsertChunk {
				chunk := part.rows[start:min(start+upsertChunk, len(part.rows))]
				q := tx.NewInsert().Model(&chunk).Returning("NULL").On("CONFLICT (user_id, source, external_id) DO UPDATE")
				for _, c := range part.cols {
					q = q.Set(c + " = EXCLUDED." + c)
				}
				if _, err := q.Exec(ctx); err != nil {
					return err
				}
			}
		}
		if len(rows) == 0 {
			return nil
		}
		return bumpBodyVersion(ctx, tx, userID)
	})
	if err != nil {
		if isFK(err) {
			return 0, store.ErrNotFound
		}
		return 0, err
	}
	return len(rs), nil
}

// lastPerExternalID drops earlier readings that share (source, external id) with a later one in
// the same batch. One INSERT must not touch the same row twice: Postgres refuses it, and SQLite
// would silently apply them in order. Readings without an external id never conflict.
func lastPerExternalID(rows []scaleRow) []scaleRow {
	type key struct{ source, id string }
	last := map[key]int{}
	for i, r := range rows {
		if r.ExternalID != nil {
			last[key{r.Source, *r.ExternalID}] = i
		}
	}
	if len(last) == 0 {
		return rows
	}
	out := rows[:0:0]
	for i, r := range rows {
		if r.ExternalID == nil || last[key{r.Source, *r.ExternalID}] == i {
			out = append(out, r)
		}
	}
	return out
}

func (s *Store) ListScaleReadings(ctx context.Context, userID int64) ([]models.ScaleReading, error) {
	var rows []scaleRow
	if err := s.db.NewSelect().Model(&rows).Where("user_id = ?", userID).
		OrderExpr("measured_at DESC, id DESC").Scan(ctx); err != nil {
		return nil, err
	}
	var out []models.ScaleReading
	for i := range rows {
		out = append(out, rows[i].toModel())
	}
	return out, nil
}

func (s *Store) DeleteScaleReading(ctx context.Context, userID, id int64) error {
	return s.deleteOwned(ctx, (*scaleRow)(nil), userID, id)
}

func (s *Store) deleteOwned(ctx context.Context, model any, userID, id int64) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewDelete().Model(model).Where("id = ?", id).Where("user_id = ?", userID).Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return store.ErrNotFound
		}
		return bumpBodyVersion(ctx, tx, userID)
	})
}
