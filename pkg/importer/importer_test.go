package importer_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/importer/withings"
)

func TestRegistry(t *testing.T) {
	r := importer.NewRegistry(withings.Importer{})
	if _, ok := r.Get("nope"); ok {
		t.Fatal("unexpected importer")
	}
	imp, ok := r.Get("withings")
	if !ok {
		t.Fatal("withings not registered")
	}
	if !reflect.DeepEqual(r.Names(), []string{"withings"}) {
		t.Fatalf("names: %v", r.Names())
	}
	if _, err := imp.Import(context.Background(), nil, 1, time.Time{}); !errors.Is(err, importer.ErrNotImplemented) {
		t.Fatalf("want ErrNotImplemented, got %v", err)
	}
	var nilReg *importer.Registry
	if _, ok := nilReg.Get("withings"); ok || nilReg.Names() != nil {
		t.Fatal("nil registry must be empty")
	}
}
