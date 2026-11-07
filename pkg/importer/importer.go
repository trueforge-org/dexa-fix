// Package importer defines how external services (e.g. Withings) feed scale readings into the store.
package importer

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// ErrNotImplemented is returned by importers that are registered but not functional yet.
var ErrNotImplemented = errors.New("not implemented")

// Importer imports measurements for a single user from an external provider.
// Implementations write through store.Store.UpsertScaleReadings with a stable Source and
// ExternalID so that repeated imports are idempotent.
type Importer interface {
	// Name is the provider name used in URLs and as the readings' Source, e.g. "withings".
	Name() string
	// Import fetches measurements taken at or after since and returns how many were stored.
	Import(ctx context.Context, st store.Store, userID int64, since time.Time) (int, error)
}

// Registry holds the known importers by name.
type Registry struct {
	byName map[string]Importer
}

// NewRegistry returns a registry containing the given importers.
func NewRegistry(imps ...Importer) *Registry {
	r := &Registry{byName: map[string]Importer{}}
	for _, i := range imps {
		r.byName[i.Name()] = i
	}
	return r
}

// Get returns the importer registered under name.
func (r *Registry) Get(name string) (Importer, bool) {
	if r == nil {
		return nil, false
	}
	i, ok := r.byName[name]
	return i, ok
}

// Names lists the registered provider names in alphabetical order.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
