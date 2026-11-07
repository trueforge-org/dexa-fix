// Package withings is a placeholder for the per-user Withings API import.
//
// Planned implementation: store the user's OAuth tokens in user_integrations (provider "withings"),
// call the Withings "Measure - Getmeas" endpoint since the given time, map the measure types
// (1 weight, 5 fat-free mass, 6 fat ratio, 8 fat mass, 76 muscle mass, 77 hydration, 88 bone mass,
// 170 visceral fat) onto models.ScaleReading, and upsert with Source "withings" and the Withings
// group id as ExternalID.
package withings

import (
	"context"
	"fmt"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// Name is the provider name, also used as the readings' Source.
const Name = "withings"

// Importer imports Withings measurements. It is not implemented yet.
type Importer struct{}

var _ importer.Importer = Importer{}

// Name returns the provider name.
func (Importer) Name() string { return Name }

// Import is not implemented yet.
func (Importer) Import(context.Context, store.Store, int64, time.Time) (int, error) {
	return 0, fmt.Errorf("withings import: %w", importer.ErrNotImplemented)
}
