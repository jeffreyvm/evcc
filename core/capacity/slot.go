package capacity

import "time"

// ExceedTolerance is how far a slot may end above target before it counts as exceeded.
// It matches the F1 acceptance criterion: no slot above target + 100 W caused by charging.
const ExceedTolerance = 100.0 // W

// slotGap is the longest stretch without a cycle before a slot result counts as partial
const slotGap = time.Minute

// SlotResult summarizes a completed slot
type SlotResult struct {
	Start   time.Time `json:"start"`
	Average float64   `json:"average"` // W, slot average import
	Vehicle float64   `json:"vehicle"` // W, slot average charge power
	Target  float64   `json:"target"`  // W, target during the slot

	// Exceeded is set when the average ends above target plus ExceedTolerance
	Exceeded bool `json:"exceeded"`
	// ByVehicle is set when the slot exceeded, but would not have without charging
	ByVehicle bool `json:"byVehicle"`
	// Partial is set when cycles were missing, e.g. after a restart, so the values are estimates
	Partial bool `json:"partial"`
}

// House returns the slot average without charging
func (r SlotResult) House() float64 {
	return r.Average - r.Vehicle
}

// Attribute decides whether a slot exceeded its target and whether charging caused it
func Attribute(average, vehicle, target float64) (exceeded, byVehicle bool) {
	limit := target + ExceedTolerance
	exceeded = average > limit
	byVehicle = exceeded && average-vehicle <= limit
	return exceeded, byVehicle
}

// SlotTracker follows the slots cycle by cycle and reports each completed slot
type SlotTracker struct {
	start   time.Time // current slot, zero before the first cycle
	at      time.Time // last cycle
	last    Budget    // budget at the last cycle
	power   float64   // W charge power at the last cycle
	vehicle float64   // kWh charged in the current slot up to the last cycle
	partial bool
}

// Update records a cycle. When the cycle starts a new slot, the previous slot is returned.
// The energy between the last cycle and the slot end is extrapolated at the last powers,
// as in the projection.
func (t *SlotTracker) Update(b Budget, chargePower float64, now time.Time) (SlotResult, bool) {
	if t.start.Equal(b.SlotStart) {
		if now.Sub(t.at) > slotGap {
			t.partial = true
		}
		// the charge power is read before the loadpoints update, so it was measured near
		// the start of the interval that just ended
		t.vehicle += kWh(chargePower, now.Sub(t.at))
		t.at, t.last, t.power = now, b, chargePower
		return SlotResult{}, false
	}

	var (
		res  SlotResult
		done bool
	)

	// only the slot right before this one completed, a longer gap has nothing to report
	if !t.start.IsZero() && t.start.Add(SlotDuration).Equal(b.SlotStart) {
		vehicle := t.vehicle + kWh(t.power, t.last.Remaining)
		res = SlotResult{
			Start:   t.start,
			Average: SlotPower(t.last.Projection),
			Vehicle: SlotPower(vehicle),
			Target:  t.last.Target,
			Partial: t.partial || t.last.Assumed > 0 || t.last.Remaining > slotGap,
		}
		res.Exceeded, res.ByVehicle = Attribute(res.Average, res.Vehicle, res.Target)
		done = true
	}

	// a new slot: charging since its start is assumed at the current power
	elapsed := now.Sub(b.SlotStart)
	t.start, t.at, t.last, t.power = b.SlotStart, now, b, chargePower
	t.vehicle = kWh(chargePower, elapsed)
	t.partial = elapsed > slotGap

	return res, done
}

// Reset forgets the current slot, e.g. after a measurement gap or a mode change
func (t *SlotTracker) Reset() {
	*t = SlotTracker{}
}
