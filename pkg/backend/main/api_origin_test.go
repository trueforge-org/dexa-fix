package mainbackend

import (
	"strings"
	"testing"
)

func TestTheAPIChecksWhereAndWhenAReadingCameFrom(t *testing.T) {
	post := func(r *jobRig, body string) (int, string) {
		t.Helper()
		s, b, _ := r.call("POST", "/scale-readings", body)
		return s, b
	}
	reading := func(extra string) string {
		return `[{"measured_at":"2026-03-01T08:00:00Z","weight_kg":80` + extra + `}]`
	}
	refused := map[string]string{
		"no time":                   `[{"weight_kg":80}]`,
		"a source in capitals":      reading(`,"source":"My Scale"`),
		"a source that is too long": reading(`,"source":"` + strings.Repeat("a", 33) + `"`),
		"a long external id":        reading(`,"source":"scale","external_id":"` + strings.Repeat("x", maxExternalIDLen+1) + `"`),
	}
	for name, body := range refused {
		t.Run("refused: "+name, func(t *testing.T) {
			r := newJobRig(t)
			if s, b := post(r, body); s != 400 || !strings.Contains(b, "item 0") {
				t.Fatalf("status %d %.120q, want 400 naming item 0", s, b)
			}
		})
	}
	accepted := map[string]string{
		"no source":               reading(``),
		"a provider's name":       reading(`,"source":"withings","external_id":"1"`),
		"the manual source":       reading(`,"source":"manual"`),
		"a source of their own":   reading(`,"source":"my-scale_2","external_id":"abc"`),
		"the longest external id": reading(`,"source":"scale","external_id":"` + strings.Repeat("x", maxExternalIDLen) + `"`),
		"the longest source":      reading(`,"source":"` + strings.Repeat("a", 32) + `"`),
	}
	for name, body := range accepted {
		t.Run("accepted: "+name, func(t *testing.T) {
			r := newJobRig(t)
			if s, b := post(r, body); s != 200 || !strings.Contains(b, `"upserted":1`) {
				t.Fatalf("status %d %.120q, want 200", s, b)
			}
		})
	}
}
