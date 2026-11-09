// Package builtin lists the workers that ship with the program, so the processes that run them
// (dexa-fix serve worker, and the single-process mode) choose providers by name without knowing
// any of them. A new provider is one more line in providers.
package builtin

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/trueforge-org/dexa-fix/pkg/shared/workerapi"
	"github.com/trueforge-org/dexa-fix/pkg/workers/runner"
	"github.com/trueforge-org/dexa-fix/pkg/workers/strengthlevel"
	"github.com/trueforge-org/dexa-fix/pkg/workers/withings"
)

// Options are what tests change; production uses the zero value.
type Options struct {
	// HTTPClient is used for outside calls; nil gives each provider its own default.
	HTTPClient *http.Client
	// WithingsAPIBase points the Withings worker at another address.
	WithingsAPIBase string
	// StrengthLevelBase points the Strength Level worker at another address.
	StrengthLevelBase string
}

// providers maps a provider's name to its handlers.
var providers = map[string]func(Options) map[string]runner.Handler{
	workerapi.ProviderWithings: func(o Options) map[string]runner.Handler {
		return withings.New(withings.Config{APIBase: o.WithingsAPIBase}, o.HTTPClient).Handlers()
	},
	workerapi.ProviderStrengthLevel: func(o Options) map[string]runner.Handler {
		return strengthlevel.New(strengthlevel.Config{Base: o.StrengthLevelBase}, o.HTTPClient).Handlers()
	},
}

// Names lists the providers, sorted.
func Names() []string {
	out := make([]string, 0, len(providers))
	for n := range providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Register adds the handlers of the named providers to r. An empty list means all of them.
func Register(r *runner.Runner, names []string, o Options) error {
	if len(names) == 0 {
		names = Names()
	}
	for _, n := range names {
		n = strings.TrimSpace(n)
		mk, ok := providers[n]
		if !ok {
			return fmt.Errorf("unknown worker provider %q (known: %s)", n, strings.Join(Names(), ", "))
		}
		for kind, h := range mk(o) {
			r.Handle(kind, h)
		}
	}
	return nil
}
