package capacity

import (
	"time"
	_ "time/tzdata" // the billing time zone must load on hosts without zoneinfo
)

// Location is the time zone of the capacity tariff. Billing months and cooking windows
// follow Belgian local time, whatever the time zone of the host.
var Location = mustLoadLocation("Europe/Brussels")

func mustLoadLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}
