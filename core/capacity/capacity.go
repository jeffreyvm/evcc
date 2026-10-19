// Package capacity keeps the quarter-hour average grid import below a target peak,
// as needed for capacity-based grid tariffs (e.g. the Flemish capaciteitstarief).
//
// The package is pure calculation: it has no evcc dependencies so all rules can be
// covered by table tests. Power is in W, energy in kWh, matching the metrics collector.
package capacity

import (
	"math"
	"time"
)

// SlotDuration is the length of a billing interval
const SlotDuration = 15 * time.Minute

// slotHours is SlotDuration in hours
const slotHours = 0.25

// Settings are the tunable parameters of the capacity controller
type Settings struct {
	Floor  float64 // W, target peak is never below this
	Margin float64 // W, subtracted from the target to absorb control delay

	MinCurrentHold  time.Duration // how long headroom must persist before restarting a paused loadpoint
	RestartHeadroom float64       // W above minimum power required for a restart
	RestartPause    time.Duration // minimum pause before a restart

	ThreatDelay  time.Duration // grid power must exceed the budget this long before a peak threat is raised
	MeterTimeout time.Duration // after this long without grid measurement the fallback applies

	CookingBattery float64 // W, battery power below which smart charging stays off grid in the cooking window
	Cooking        Windows
}

// DefaultSettings returns the defaults from the design document
func DefaultSettings() Settings {
	return Settings{
		Floor:           2500,
		Margin:          150,
		MinCurrentHold:  120 * time.Second,
		RestartHeadroom: 300,
		RestartPause:    5 * time.Minute,
		ThreatDelay:     30 * time.Second,
		MeterTimeout:    30 * time.Second,
		CookingBattery:  2000,
		Cooking:         DefaultWindows(),
	}
}

// SlotStart returns the start of the billing interval containing t.
// Quarter boundaries are the same in UTC and local time for whole-hour and half-hour zones.
func SlotStart(t time.Time) time.Time {
	return t.Truncate(SlotDuration)
}

// MonthStart returns the first instant of the calendar month containing t in loc
func MonthStart(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), 1, 0, 0, 0, 0, loc)
}

// Target returns the target average import power for the current slot:
// the higher of floor and the month peak so far, minus the control margin.
// The target is never raised by anything else.
func Target(s Settings, monthPeak float64) float64 {
	return math.Max(s.Floor, monthPeak) - s.Margin
}

// Measurement is the grid state at one control cycle
type Measurement struct {
	Time      time.Time
	GridPower float64 // W, positive is import

	// SlotEnergy is the gross import in kWh counted since CountedFrom within the current slot.
	// Export does not reduce it, like the grid operator's quarter-hour register.
	SlotEnergy  float64
	CountedFrom time.Time // zero or slot start when the whole slot was counted

	MonthPeak float64 // W, highest completed slot average this month, 0 if none
}

// Budget is the derived state of the current billing interval
type Budget struct {
	Target    float64       // W
	SlotStart time.Time     // start of the current slot
	Remaining time.Duration // time left in the slot

	Energy  float64 // kWh imported so far, including the assumption for uncounted time
	Assumed float64 // kWh of Energy that was assumed because counting started late

	// Rest is the average import power still allowed until the end of the slot,
	// capped at Target so a nearly empty slot does not allow a burst that spills over
	Rest float64 // W

	Projection float64 // kWh at slot end if the current grid power continues
}

// ProjectedPower returns the projected slot average in W
func (b Budget) ProjectedPower() float64 {
	return b.Projection * 1e3 / slotHours
}

// AveragePower returns the slot average so far in W, over the elapsed part of the slot
func (b Budget) AveragePower() float64 {
	elapsed := SlotDuration - b.Remaining
	if elapsed <= 0 {
		return 0
	}
	return b.Energy * 1e3 / elapsed.Hours()
}

// Exceeded reports whether the projected slot average is above the target
func (b Budget) Exceeded() bool {
	return b.ProjectedPower() > b.Target
}

// Compute derives the slot budget from a measurement
func Compute(s Settings, m Measurement) Budget {
	start := SlotStart(m.Time)
	end := start.Add(SlotDuration)
	target := Target(s, m.MonthPeak)

	b := Budget{
		Target:    target,
		SlotStart: start,
		Remaining: end.Sub(m.Time),
		Energy:    math.Max(0, m.SlotEnergy),
	}

	// after a restart mid-slot, assume import at target until counting started
	if m.CountedFrom.After(start) {
		uncounted := minTime(m.CountedFrom, m.Time).Sub(start)
		b.Assumed = target * uncounted.Hours() / 1e3
		b.Energy += b.Assumed
	}

	allowed := target * slotHours / 1e3 // kWh
	left := allowed - b.Energy

	// guard against division by a vanishing remainder in the last second of the slot
	remaining := math.Max(b.Remaining.Hours(), time.Second.Hours())

	b.Rest = math.Min(target, math.Max(0, left*1e3/remaining))
	b.Projection = b.Energy + math.Max(0, m.GridPower)*b.Remaining.Hours()/1e3

	return b
}

// HoldsAt reports whether the slot ends at or below target if grid power changes by delta W
// for the rest of the slot. Used to decide whether a loadpoint may stay at minimum current.
func (b Budget) HoldsAt(gridPower, delta float64) bool {
	end := b.Energy + math.Max(0, gridPower+delta)*b.Remaining.Hours()/1e3
	return end <= b.Target*slotHours/1e3
}
