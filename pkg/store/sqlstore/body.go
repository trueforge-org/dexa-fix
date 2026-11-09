package sqlstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// ---- body: dexa scans and scale readings ----

// The measured values and notes of a row are one JSON document, sealed with the body cipher. The
// context names the user and the row's row_uid, so a value copied into another row does not open.
// Without an encryption key the document stays readable, like the other sensitive columns.

// newRowUID returns the random token that goes into the encryption context of a body row. It is
// new on every write, so a sealed value is only ever valid for the write that made it.
func newRowUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *Store) sealScale(r *scaleRow) (err error) {
	if r.RowUID, err = newRowUID(); err != nil {
		return err
	}
	raw, err := json.Marshal(r.Vals)
	if err != nil {
		return err
	}
	r.Data, err = sealWith(s.bodyCipher, string(raw), scaleContext(r.UserID, r.RowUID))
	return err
}

func (s *Store) sealDexa(r *dexaRow) (err error) {
	if r.RowUID, err = newRowUID(); err != nil {
		return err
	}
	raw, err := json.Marshal(r.Vals)
	if err != nil {
		return err
	}
	r.Data, err = sealWith(s.bodyCipher, string(raw), dexaContext(r.UserID, r.RowUID))
	return err
}

// openScale reads the document of a row into Vals.
func (s *Store) openScale(r *scaleRow) error {
	plain, err := openWith(s.bodyCipher, r.Data, scaleContext(r.UserID, r.RowUID))
	if err != nil {
		return fmt.Errorf("scale reading %d: %w", r.ID, err)
	}
	r.Vals = scaleValues{}
	if err := json.Unmarshal([]byte(plain), &r.Vals); err != nil {
		return fmt.Errorf("scale reading %d: %w", r.ID, err)
	}
	return nil
}

func (s *Store) openDexa(r *dexaRow) error {
	plain, err := openWith(s.bodyCipher, r.Data, dexaContext(r.UserID, r.RowUID))
	if err != nil {
		return fmt.Errorf("dexa scan %d: %w", r.ID, err)
	}
	r.Vals = dexaValues{}
	if err := json.Unmarshal([]byte(plain), &r.Vals); err != nil {
		return fmt.Errorf("dexa scan %d: %w", r.ID, err)
	}
	return nil
}

func (s *Store) scaleModel(r *scaleRow) (models.ScaleReading, error) {
	if err := s.openScale(r); err != nil {
		return models.ScaleReading{}, err
	}
	v := r.Vals
	return models.ScaleReading{
		ID: r.ID, UserID: r.UserID, MeasuredAt: r.MeasuredAt.UTC(),
		WeightKg: v.WeightKg, FatRatioPct: v.FatRatioPct, FatMassKg: v.FatMassKg, FatFreeMassKg: v.FatFreeMassKg,
		MuscleMassKg: v.MuscleMassKg, HydrationKg: v.HydrationKg, BoneMassKg: v.BoneMassKg, VisceralFatIndex: v.VisceralFatIndex,
		Source: r.Source, Method: r.Method, AthleteMode: r.AthleteMode, ExternalID: r.ExternalID,
		Notes: v.Notes, CreatedAt: r.CreatedAt,
	}, nil
}

func (s *Store) dexaModel(r *dexaRow) (models.DexaScan, error) {
	if err := s.openDexa(r); err != nil {
		return models.DexaScan{}, err
	}
	v := r.Vals
	return models.DexaScan{
		ID: r.ID, UserID: r.UserID, MeasuredAt: r.MeasuredAt.UTC(),
		WeightKg: v.WeightKg, FatRatioPct: v.FatRatioPct, FatMassKg: v.FatMassKg, FatFreeMassKg: v.FatFreeMassKg,
		LeanMassKg: v.LeanMassKg, BoneMassKg: v.BoneMassKg, VisceralFatMassKg: v.VisceralFatMassKg,
		BMDGCm2: v.BMDGCm2, TScore: v.TScore, ZScore: v.ZScore, AndroidFatPct: v.AndroidFatPct, GynoidFatPct: v.GynoidFatPct,
		Notes: v.Notes, CreatedAt: r.CreatedAt,
	}, nil
}

func (s *Store) scaleModels(rows []scaleRow) ([]models.ScaleReading, error) {
	var out []models.ScaleReading
	for i := range rows {
		m, err := s.scaleModel(&rows[i])
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func (s *Store) dexaModels(rows []dexaRow) ([]models.DexaScan, error) {
	var out []models.DexaScan
	for i := range rows {
		m, err := s.dexaModel(&rows[i])
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func (s *Store) CreateDexaScan(ctx context.Context, d *models.DexaScan) error {
	d.MeasuredAt = d.MeasuredAt.UTC().Truncate(24 * time.Hour)
	d.CreatedAt = now()
	row := dexaRow{
		UserID: d.UserID, MeasuredAt: d.MeasuredAt, CreatedAt: d.CreatedAt,
		Vals: dexaValues{
			WeightKg: d.WeightKg, FatRatioPct: d.FatRatioPct, FatMassKg: d.FatMassKg, FatFreeMassKg: d.FatFreeMassKg,
			LeanMassKg: d.LeanMassKg, BoneMassKg: d.BoneMassKg, VisceralFatMassKg: d.VisceralFatMassKg,
			BMDGCm2: d.BMDGCm2, TScore: d.TScore, ZScore: d.ZScore, AndroidFatPct: d.AndroidFatPct, GynoidFatPct: d.GynoidFatPct,
			Notes: d.Notes,
		},
	}
	if err := s.sealDexa(&row); err != nil {
		return err
	}
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return err
		}
		return bumpBodyVersion(ctx, tx, d.UserID)
	})
	if err != nil {
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
	return s.dexaModels(rows)
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
			Source: r.Source, Method: r.Method, AthleteMode: r.AthleteMode, ExternalID: r.ExternalID,
			CreatedAt: now(),
			Vals: scaleValues{
				WeightKg: r.WeightKg, FatRatioPct: r.FatRatioPct, FatMassKg: r.FatMassKg, FatFreeMassKg: r.FatFreeMassKg,
				MuscleMassKg: r.MuscleMassKg, HydrationKg: r.HydrationKg, BoneMassKg: r.BoneMassKg, VisceralFatIndex: r.VisceralFatIndex,
				Notes: r.Notes,
			},
		})
	}
	rows = lastPerExternalID(rows)

	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := s.keepNotes(ctx, tx, userID, rows); err != nil {
			return err
		}
		for i := range rows {
			if err := s.sealScale(&rows[i]); err != nil {
				return err
			}
		}
		// An update refreshes the time and the document; method and athlete mode are user choices
		// that must survive a re-import, so they are not in the list.
		for start := 0; start < len(rows); start += upsertChunk {
			chunk := rows[start:min(start+upsertChunk, len(rows))]
			if _, err := tx.NewInsert().Model(&chunk).Returning("NULL").
				On("CONFLICT (user_id, source, external_id) DO UPDATE").
				Set("measured_at = EXCLUDED.measured_at").
				Set("row_uid = EXCLUDED.row_uid").
				Set("data = EXCLUDED.data").Exec(ctx); err != nil {
				return err
			}
		}
		if len(rows) == 0 {
			return nil
		}
		return bumpBodyVersion(ctx, tx, userID)
	})
	if err != nil {
		return 0, err
	}
	return len(rs), nil
}

// keepNotes gives a re-delivered reading that carries no notes (every Withings import) the note the
// user added in the app, which would otherwise be wiped. The notes are inside the encrypted
// document, so this reads the existing rows instead of leaving a column out of the update. A
// delivery that does carry notes replaces them.
func (s *Store) keepNotes(ctx context.Context, tx bun.Tx, userID int64, rows []scaleRow) error {
	bySource := map[string][]string{}
	for _, r := range rows {
		if r.ExternalID != nil && r.Vals.Notes == "" {
			bySource[r.Source] = append(bySource[r.Source], *r.ExternalID)
		}
	}
	if len(bySource) == 0 {
		return nil
	}
	type key struct{ source, id string }
	notes := map[key]string{}
	for source, ids := range bySource {
		for start := 0; start < len(ids); start += upsertChunk {
			var existing []scaleRow
			if err := tx.NewSelect().Model(&existing).Where("user_id = ?", userID).Where("source = ?", source).
				Where("external_id IN (?)", bun.In(ids[start:min(start+upsertChunk, len(ids))])).Scan(ctx); err != nil {
				return err
			}
			for i := range existing {
				if err := s.openScale(&existing[i]); err != nil {
					return err
				}
				if n := existing[i].Vals.Notes; n != "" && existing[i].ExternalID != nil {
					notes[key{source, *existing[i].ExternalID}] = n
				}
			}
		}
	}
	for i := range rows {
		if r := &rows[i]; r.ExternalID != nil && r.Vals.Notes == "" {
			r.Vals.Notes = notes[key{r.Source, *r.ExternalID}]
		}
	}
	return nil
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
	return s.scaleModels(rows)
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
