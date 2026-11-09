// Package timezones lists the common IANA time zones and validates a name.
package timezones

import (
	"errors"
	"strings"
	"time"
)

// Common lists the zones suggested in the time zone field; any IANA name is accepted.
var Common = []string{
	"UTC",
	"Europe/London", "Europe/Dublin", "Europe/Lisbon", "Europe/Paris", "Europe/Berlin", "Europe/Amsterdam",
	"Europe/Brussels", "Europe/Madrid", "Europe/Rome", "Europe/Zurich", "Europe/Vienna", "Europe/Stockholm",
	"Europe/Copenhagen", "Europe/Oslo", "Europe/Helsinki", "Europe/Warsaw", "Europe/Prague", "Europe/Athens",
	"Europe/Istanbul", "Europe/Kyiv", "Europe/Moscow",
	"America/St_Johns", "America/Halifax", "America/New_York", "America/Toronto", "America/Chicago",
	"America/Denver", "America/Phoenix", "America/Los_Angeles", "America/Anchorage", "Pacific/Honolulu",
	"America/Mexico_City", "America/Bogota", "America/Lima", "America/Sao_Paulo", "America/Argentina/Buenos_Aires",
	"Africa/Casablanca", "Africa/Lagos", "Africa/Cairo", "Africa/Nairobi", "Africa/Johannesburg",
	"Asia/Jerusalem", "Asia/Dubai", "Asia/Tehran", "Asia/Karachi", "Asia/Kolkata", "Asia/Dhaka", "Asia/Bangkok",
	"Asia/Jakarta", "Asia/Singapore", "Asia/Hong_Kong", "Asia/Shanghai", "Asia/Manila", "Asia/Seoul", "Asia/Tokyo",
	"Australia/Perth", "Australia/Adelaide", "Australia/Brisbane", "Australia/Sydney", "Pacific/Auckland",
}

// ErrInvalid is returned by Parse for a name that is not a usable time zone.
var ErrInvalid = errors.New("Choose a valid time zone, such as Europe/Amsterdam.")

// Parse validates an IANA time zone name. "Local" is refused: it would mean the server's.
func Parse(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" || len(name) > 64 || strings.EqualFold(name, "local") {
		return "", ErrInvalid
	}
	if _, err := time.LoadLocation(name); err != nil {
		return "", ErrInvalid
	}
	return name, nil
}
