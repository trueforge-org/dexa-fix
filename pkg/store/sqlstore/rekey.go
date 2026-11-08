package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/uptrace/bun"

	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// RekeyStats counts what Rekey changed (or, in a dry run, would change). Current counts values
// that were already sealed with the current key and are left alone.
type RekeyStats struct {
	Emails       int // user e-mail addresses re-encrypted
	Integrations int
	Settings     int
	Providers    int
	Lookups      int // e-mail lookup hashes recomputed
	Current      int // encrypted values already under the current key
}

// Changed is the number of values that were or would be rewritten.
func (r RekeyStats) Changed() int {
	return r.Emails + r.Integrations + r.Settings + r.Providers + r.Lookups
}

// Rekey re-encrypts every encrypted value with the current key, recomputes the e-mail lookup
// hashes (they derive from the key) and writes the new key check value, all in one transaction: it
// completes or leaves the database untouched. Values already sealed with the current key are
// skipped, so a run can be repeated. Any value that no configured key can open fails the whole run.
// With dryRun nothing is written and the result reports what would change.
func (s *Store) Rekey(ctx context.Context, dryRun bool) (RekeyStats, error) {
	var stats RekeyStats
	if s.cipher == nil {
		return stats, errors.New("rekey needs an encryption key")
	}
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		stats = RekeyStats{}

		// reseal returns the value sealed with the current key, or "" when it is already there.
		reseal := func(kind, value, sealCtx string) (string, error) {
			if !strings.HasPrefix(value, encPrefix) {
				return "", nil
			}
			plain, e, err := s.cipher.open(value, sealCtx)
			if err != nil {
				return "", fmt.Errorf("%s: %w", kind, err)
			}
			if e == s.cipher.current && strings.HasPrefix(value, encV2) {
				stats.Current++
				return "", nil
			}
			return s.cipher.seal(plain, sealCtx)
		}

		var integrations []integrationRow
		if err := tx.NewSelect().Model(&integrations).Scan(ctx); err != nil {
			return err
		}
		for _, r := range integrations {
			enc, err := reseal(fmt.Sprintf("integration of user %d (%s)", r.UserID, r.Provider), r.Data, integrationContext(r.UserID, r.Provider))
			if err != nil {
				return err
			}
			if enc == "" {
				continue
			}
			stats.Integrations++
			if dryRun {
				continue
			}
			if _, err := tx.NewUpdate().Model((*integrationRow)(nil)).Set("data = ?", enc).
				Where("user_id = ?", r.UserID).Where("provider = ?", r.Provider).Exec(ctx); err != nil {
				return err
			}
		}

		var settings []settingRow
		if err := tx.NewSelect().Model(&settings).Scan(ctx); err != nil {
			return err
		}
		for _, r := range settings {
			if r.Name == checkSetting {
				continue
			}
			enc, err := reseal("setting "+r.Name, r.Value, settingContext(r.Name))
			if err != nil {
				return err
			}
			if enc == "" {
				continue
			}
			stats.Settings++
			if dryRun {
				continue
			}
			if _, err := tx.NewUpdate().Model((*settingRow)(nil)).Set("value = ?", enc).Where("name = ?", r.Name).Exec(ctx); err != nil {
				return err
			}
		}

		var providers []providerRow
		if err := tx.NewSelect().Model(&providers).Scan(ctx); err != nil {
			return err
		}
		for _, r := range providers {
			enc, err := reseal("sign-in provider "+r.Slug, r.ClientSecret, providerContext(r.Slug))
			if err != nil {
				return err
			}
			if enc == "" {
				continue
			}
			stats.Providers++
			if dryRun {
				continue
			}
			if _, err := tx.NewUpdate().Model((*providerRow)(nil)).Set("client_secret = ?", enc).Where("slug = ?", r.Slug).Exec(ctx); err != nil {
				return err
			}
		}

		var users []userRow
		if err := tx.NewSelect().Model(&users).OrderExpr("id").Scan(ctx); err != nil {
			return err
		}
		for _, r := range users {
			if r.Email == "" {
				continue
			}
			kind := fmt.Sprintf("e-mail of user %d", r.ID)
			enc, err := reseal(kind, r.Email, emailContext(r.ID))
			if err != nil {
				return err
			}
			email, err := s.decrypt(r.Email, emailContext(r.ID))
			if err != nil {
				return fmt.Errorf("%s: %w", kind, err)
			}
			lookup := s.emailLookup(models.NormalizeEmail(email))
			lookupChanged := lookup != nil && (r.EmailLookup == nil || *r.EmailLookup != *lookup)
			if enc == "" && !lookupChanged {
				continue
			}
			if enc != "" {
				stats.Emails++
			}
			if lookupChanged {
				stats.Lookups++
			}
			if dryRun {
				continue
			}
			q := tx.NewUpdate().Model((*userRow)(nil)).Where("id = ?", r.ID)
			if enc != "" {
				q = q.Set("email = ?", enc)
			}
			if lookupChanged {
				q = q.Set("email_lookup = ?", lookup)
			}
			if _, err := q.Exec(ctx); err != nil {
				return err
			}
		}

		if dryRun {
			return nil
		}
		return s.writeCheck(ctx, tx)
	})
	if err != nil {
		return RekeyStats{}, err
	}
	return stats, nil
}
