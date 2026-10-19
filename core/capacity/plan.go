package capacity

import (
	"math"
	"time"
)

// PlanInput describes a charging plan to estimate against the target
type PlanInput struct {
	Now      time.Time // in the site's local time zone
	Deadline time.Time
	Budget   Budget // current slot

	HomePower func(slot time.Time) float64 // W, expected home consumption without the vehicle

	MinPower, MaxPower float64 // W of the loadpoint
}

// PlanEnergy estimates the energy in kWh a plan can charge until the deadline without
// raising the peak. Per slot the loadpoint gets the target minus expected home power,
// at minimum current inside a cooking window, nothing when that is below minimum power.
// Plans charge like fast mode, so the fast cooking rule applies.
func PlanEnergy(s Settings, in PlanInput) float64 {
	if !in.Deadline.After(in.Now) {
		return 0
	}

	var energy float64

	for slot := SlotStart(in.Now); slot.Before(in.Deadline); slot = slot.Add(SlotDuration) {
		from := maxTime(slot, in.Now)
		to := minTime(slot.Add(SlotDuration), in.Deadline)

		avail := in.Budget.Target - in.HomePower(slot)
		if slot.Equal(in.Budget.SlotStart) {
			// current slot: what is left of the budget, at the current home power
			avail = in.Budget.Rest - in.HomePower(slot)
		}

		local := from.In(in.Now.Location())
		if _, cooking := s.Cooking.Active(local); cooking {
			avail = math.Min(avail, in.MinPower)
		}

		if avail < in.MinPower {
			continue
		}

		energy += math.Min(avail, in.MaxPower) * to.Sub(from).Hours() / 1e3
	}

	return energy
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
