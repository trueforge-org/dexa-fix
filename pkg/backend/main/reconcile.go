package mainbackend

import "github.com/trueforge-org/dexafix/pkg/shared/bodyapi"

// A source of scale readings (Withings, and whatever comes after it) hands the backend what it
// found. The backend passes it to the body service (bodyService.Reconcile), which holds the readings
// and decides what to store: see pkg/backend/body. A worker only fetches; it never sees the user's
// stored readings.

// importResult reports what an import did.
type importResult = bodyapi.ImportResult
