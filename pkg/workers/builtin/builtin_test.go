package builtin

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/shared/workerapi"
	"github.com/trueforge-org/dexafix/pkg/workers/runner"
)

func newRunner(t *testing.T) *runner.Runner {
	t.Helper()
	c, err := workerapi.New("http://127.0.0.1:1", nil, "secret")
	if err != nil {
		t.Fatal(err)
	}
	return runner.New(c, runner.Options{})
}

// handled reports whether r has any handler: Run refuses to start without one, and with a cancelled
// context it otherwise ends at once without an error.
func handled(r *runner.Runner) bool {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return r.Run(ctx) == nil
}

func TestTheBuiltInProvidersAreListedSorted(t *testing.T) {
	want := []string{workerapi.ProviderStrengthLevel, workerapi.ProviderWithings}
	if got := Names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Names = %v, want %v", got, want)
	}
}

func TestRegisterGivesTheRunnerTheHandlersOfTheNamedProviders(t *testing.T) {
	for name, names := range map[string][]string{
		"no names means all providers": nil,
		"one provider by name":         {" " + workerapi.ProviderWithings + " "},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRunner(t)
			if err := Register(r, names, Options{}); err != nil {
				t.Fatal(err)
			}
			if !handled(r) {
				t.Fatal("the runner has no handlers")
			}
		})
	}
}

func TestRegisterRefusesAnUnknownProviderAndNamesTheKnownOnes(t *testing.T) {
	r := newRunner(t)
	err := Register(r, []string{"fitbit"}, Options{})
	if err == nil || !strings.Contains(err.Error(), `unknown worker provider "fitbit"`) || !strings.Contains(err.Error(), "strengthlevel, withings") {
		t.Fatalf("got %v", err)
	}
	if handled(r) {
		t.Fatal("a refused name must not register anything")
	}
}
