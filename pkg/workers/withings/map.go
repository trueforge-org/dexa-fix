package withings

import (
	"math"
	"strconv"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// attribUnsure is Withings' "measurement may belong to someone else" attribution (e.g. a guest on the scale).
const attribUnsure = 1

// Withings attributions for a value somebody typed into the app instead of a device measuring it:
// 2 is a manual entry, 4 a manual entry made while creating the user. Such a value says nothing
// about the scale, so it gets its own method and is never mixed into (or calibrated as) the
// scale's own readings.
const (
	attribManual            = 2
	attribManualUserCreated = 4
)

// mapGroups converts Withings groups to scale readings. seen holds the id of every group the API
// returned, including the ones that were dropped (guest measurements, groups without mapped
// types), so a day re-import never deletes a stored reading just because it is now filtered out.
func mapGroups(groups []measureGroup) (rs []models.ScaleReading, seen map[string]bool) {
	seen = make(map[string]bool, len(groups))
	for _, g := range groups {
		id := strconv.FormatInt(g.GrpID, 10)
		seen[id] = true
		if g.Category != 1 || g.Attrib == attribUnsure || g.Date <= 0 {
			continue
		}
		method := models.MethodScale
		if g.Attrib == attribManual || g.Attrib == attribManualUserCreated {
			method = models.MethodManualEntry
		}
		r := models.ScaleReading{Source: Provider, Method: method, ExternalID: &id, MeasuredAt: time.Unix(g.Date, 0).UTC()}
		mapped := 0
		for _, m := range g.Measures {
			v := math.Round(float64(m.Value)*math.Pow10(m.Unit)*1e6) / 1e6
			var dst **float64
			switch m.Type {
			case 1:
				dst = &r.WeightKg
			case 5:
				dst = &r.FatFreeMassKg
			case 6:
				dst = &r.FatRatioPct
			case 8:
				dst = &r.FatMassKg
			case 76:
				dst = &r.MuscleMassKg
			case 77:
				dst = &r.HydrationKg
			case 88:
				dst = &r.BoneMassKg
			case 170:
				dst = &r.VisceralFatIndex
			default:
				continue
			}
			*dst = &v
			mapped++
		}
		if mapped > 0 {
			rs = append(rs, r)
		}
	}
	return rs, seen
}
