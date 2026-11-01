package capacity

import (
	"cmp"
	"math"
	"slices"
	"time"
)

// ReserveMode selects how the reserve target is set
type ReserveMode string

const (
	ReserveDynamic ReserveMode = "dynamic" // season floor or the expected house peaks, whichever is higher
	ReserveFixed   ReserveMode = "fixed"   // season floor only
)

// ReserveModes are the valid reserve modes
var ReserveModes = []ReserveMode{ReserveDynamic, ReserveFixed}

// ReserveSettings are the tunable parameters of the reserve and the grid charge planner
type ReserveSettings struct {
	Mode         ReserveMode
	SeasonFloors [12]float64   // % of the capacity per month, January first
	Deadline     time.Duration // since local midnight, the reserve must be full by then
	Buffer       float64       // kWh for unexpected peaks on top of the expected ones
	Efficiency   float64       // charge and discharge, one way
	MinDays      int           // days of house profile needed for the dynamic reserve
	SolarMin     float64       // W, a slot with more expected solar power is no grid charge candidate
	RoomStep     float64       // W, candidates with room within this step count as equal
}

// DefaultSeasonFloors are 30% from November to February, 15% in March, April, September and
// October, 0% from May to August
var DefaultSeasonFloors = [12]float64{30, 30, 15, 15, 0, 0, 0, 0, 15, 15, 30, 30}

// DefaultReserveSettings returns the default reserve settings
func DefaultReserveSettings() ReserveSettings {
	return ReserveSettings{
		Mode:         ReserveDynamic,
		SeasonFloors: DefaultSeasonFloors,
		Deadline:     17 * time.Hour,
		Buffer:       1,
		Efficiency:   0.95,
		MinDays:      7,
		SolarMin:     100,
		RoomStep:     100,
	}
}

// ReserveInput is everything the reserve planner needs. Profiles and forecast are per slot
// starting with the current slot.
type ReserveInput struct {
	Now    time.Time // in Location
	Units  BatteryUnits
	Target float64 // W target peak after the margin, as the budget uses it

	HouseP90    []float64 // W per slot, house without loadpoints, 90th percentile
	HouseP50    []float64 // W per slot, median
	ProfileDays int       // days of house history behind the profiles

	Solar      []float64 // W per slot, nil without forecast
	SolarScale float64   // ratio of actual to forecast solar energy, 1 if unknown

	VehicleCharging bool // a loadpoint charges now: the current slot is no candidate
}

// ChargeSlot is a slot in which units charge from the grid
type ChargeSlot struct {
	Start time.Time `json:"start"`
	Units []string  `json:"units"`
	Power float64   `json:"power"` // W combined charge power
}

// ReservePlan is the result of the grid charge planner
type ReservePlan struct {
	Updated  time.Time    `json:"updated"`
	Deadline time.Time    `json:"deadline"`
	Reserve  float64      `json:"reserve"`  // kWh target incl. the unit minimums
	Season   float64      `json:"season"`   // kWh season floor part
	Peaks    float64      `json:"peaks"`    // kWh the expected house peaks after the deadline need, incl. the buffer
	Energy   float64      `json:"energy"`   // kWh stored now
	Surplus  float64      `json:"surplus"`  // kWh expected from solar until the deadline
	Need     float64      `json:"need"`     // kWh to charge from the grid
	Fixed    bool         `json:"fixed"`    // reserve from the season floor only, e.g. too little profile history
	Slots    []ChargeSlot `json:"slots"`    // planned grid charge slots, in time order
	Shortage float64      `json:"shortage"` // kWh the candidate slots cannot cover
}

// Units returns the units planned to charge in the slot starting at t
func (p ReservePlan) Units(t time.Time) map[string]bool {
	slot := SlotStart(t)
	for _, s := range p.Slots {
		if s.Start.Equal(slot) {
			res := make(map[string]bool, len(s.Units))
			for _, u := range s.Units {
				res[u] = true
			}
			return res
		}
	}
	return nil
}

// slotAt returns the value of a per slot series i slots ahead, 0 beyond its end
func slotAt(series []float64, i int) float64 {
	if i < 0 || i >= len(series) {
		return 0
	}
	return series[i]
}

// deadline returns the first slot the reserve may be needed in: the configured deadline, or
// the first slot before it where the house alone is expected above target. After the
// deadline it is the deadline of the next day.
func (s ReserveSettings) deadline(in ReserveInput) time.Time {
	now := in.Now
	l := now.In(Location)
	day := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, Location)

	at := func(d time.Time) time.Time {
		return time.Date(d.Year(), d.Month(), d.Day(), int(s.Deadline.Hours()), int(s.Deadline.Minutes())%60, 0, 0, Location)
	}

	dl := at(day)
	if !now.Before(dl) {
		dl = at(day.AddDate(0, 0, 1))
	}

	slot := SlotStart(now)
	for i := 0; slot.Add(time.Duration(i) * SlotDuration).Before(dl); i++ {
		if slotAt(in.HouseP90, i) > in.Target {
			// the current slot needs the reserve already, there is nothing to plan for it
			return slot.Add(time.Duration(i) * SlotDuration)
		}
	}

	return dl
}

// ReserveTarget returns the reserve target in kWh incl. the unit minimums, its season floor
// part, and the part for the expected house peaks after the deadline incl. the buffer:
//
//	R = max(R_season, 1/η × Σ max(0, P90(q) − target) × 0.25h + R_buffer) + R_min
//
// The peaks are summed from the deadline to the end of that day. With the fixed mode or too
// little profile history only the season floor counts.
func (s ReserveSettings) ReserveTarget(in ReserveInput, deadline time.Time) (reserve, season, peaks float64, fixed bool) {
	capacity, minimum := in.Units.Capacity(), in.Units.MinEnergy()
	// the floor of the month the reserve is needed in: at 22:00 on 31 October already November's
	month := deadline.In(Location).Month()
	season = s.SeasonFloors[month-1] / 100 * capacity

	fixed = s.Mode == ReserveFixed || in.ProfileDays < s.MinDays
	if !fixed {
		l := deadline.In(Location)
		end := time.Date(l.Year(), l.Month(), l.Day()+1, 0, 0, 0, 0, Location)
		first := int(deadline.Sub(SlotStart(in.Now)) / SlotDuration)
		last := int(end.Sub(SlotStart(in.Now)) / SlotDuration)

		var sum float64
		for i := first; i < last; i++ {
			sum += math.Max(0, slotAt(in.HouseP90, i)-in.Target)
		}
		peaks = kWh(sum, SlotDuration)/s.Efficiency + s.Buffer
	}

	reserve = math.Min(capacity, math.Max(season, peaks)+minimum)
	return reserve, season, peaks, fixed
}

// Surplus returns the solar energy in kWh expected to charge the batteries until the deadline:
//
//	S = η × Σ max(0, k × Ppv(q) − P50(q)) × 0.25h
//
// The current slot counts only for its remaining part. Without forecast S is 0.
func (s ReserveSettings) Surplus(in ReserveInput, deadline time.Time) float64 {
	if len(in.Solar) == 0 {
		return 0
	}

	k := solarScale(in.SolarScale)

	slot := SlotStart(in.Now)
	var sum float64
	for i := 0; slot.Add(time.Duration(i) * SlotDuration).Before(deadline); i++ {
		d := SlotDuration
		if i == 0 {
			d = slot.Add(SlotDuration).Sub(in.Now)
		}
		sum += kWh(math.Max(0, k*slotAt(in.Solar, i)-slotAt(in.HouseP50, i)), d)
	}

	return s.Efficiency * sum
}

// solarScale returns a usable forecast scale, 1 if unknown
func solarScale(k float64) float64 {
	if k <= 0 || math.IsNaN(k) || math.IsInf(k, 0) {
		return 1
	}
	return k
}

// b2i orders true before false when compared descending
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// candidate is a slot grid charging may use
type candidate struct {
	start time.Time
	room  float64 // W the house leaves below target
	part  float64 // part of the slot still ahead, 1 except for the current slot
}

// unitNeed is the grid energy one unit still needs
type unitNeed struct {
	unit BatteryUnit
	need float64 // kWh into the unit
}

// PlanGridCharge plans grid charging so the reserve is full at the deadline: the need
// G = max(0, R − E − S) is split over the units by their share of the reserve, and slots
// before the deadline without expected solar and with room for the charge power are chosen.
// Slots that fit all units needing energy come first, then the ones with most room, then the
// latest. If not all units fit, the largest unit that fits charges alone.
func (s ReserveSettings) PlanGridCharge(in ReserveInput) ReservePlan {
	units := in.Units.reachable()

	dl := s.deadline(in)
	reserve, season, peaks, fixed := s.ReserveTarget(in, dl)

	p := ReservePlan{
		Updated:  in.Now,
		Deadline: dl,
		Reserve:  reserve,
		Season:   season,
		Peaks:    peaks,
		Energy:   units.Energy(),
		Fixed:    fixed,
	}
	p.Surplus = s.Surplus(in, dl)
	p.Need = math.Max(0, reserve-p.Energy-p.Surplus)

	if p.Need <= 0 || len(units) == 0 {
		return p
	}

	// each unit gets its share of the reserve, the surplus split by capacity, never above its max soc
	var needs []*unitNeed
	capacity := units.Capacity()
	for _, u := range units {
		need := units.share(u, reserve) - u.Energy() - p.Surplus*u.Capacity/capacity
		need = math.Min(need, u.maxSoc()/100*u.Capacity-u.Energy())
		if need > 0 && u.MaxPower > 0 {
			needs = append(needs, &unitNeed{unit: u, need: need})
		}
	}

	var candidates []candidate
	k := solarScale(in.SolarScale)
	slot := SlotStart(in.Now)
	for i := 0; slot.Add(time.Duration(i) * SlotDuration).Before(dl); i++ {
		if (i == 0 && in.VehicleCharging) || k*slotAt(in.Solar, i) > s.SolarMin {
			continue
		}
		start := slot.Add(time.Duration(i) * SlotDuration)
		part := 1.0
		if i == 0 {
			part = float64(start.Add(SlotDuration).Sub(in.Now)) / float64(SlotDuration)
		}
		room := in.Target - slotAt(in.HouseP90, i)
		candidates = append(candidates, candidate{start: start, room: room, part: part})
	}

	var total float64
	for _, n := range needs {
		total += n.unit.MaxPower
	}

	// coarse room: candidates within one step count as equal, so the latest one wins the tie
	step := math.Max(1, s.RoomStep)
	slices.SortStableFunc(candidates, func(a, b candidate) int {
		if c := cmp.Compare(b2i(b.room >= total), b2i(a.room >= total)); c != 0 {
			return c
		}
		if c := cmp.Compare(math.Floor(b.room/step), math.Floor(a.room/step)); c != 0 {
			return c
		}
		return b.start.Compare(a.start)
	})

	for _, c := range candidates {
		var open []*unitNeed
		for _, n := range needs {
			if n.need > 1e-6 {
				open = append(open, n)
			}
		}
		if len(open) == 0 {
			break
		}

		chosen := fitUnits(open, c.room, func(n *unitNeed) float64 { return n.unit.MaxPower })
		if len(chosen) == 0 {
			continue
		}

		ps := ChargeSlot{Start: c.start}
		for _, n := range chosen {
			n.need -= kWh(n.unit.MaxPower, SlotDuration) * s.Efficiency * c.part
			ps.Units = append(ps.Units, n.unit.Name)
			ps.Power += n.unit.MaxPower
		}
		p.Slots = append(p.Slots, ps)
	}

	for _, n := range needs {
		p.Shortage += math.Max(0, n.need)
	}

	slices.SortFunc(p.Slots, func(a, b ChargeSlot) int { return a.Start.Compare(b.Start) })
	return p
}

// fitUnits returns the units that charge in a slot with the given room: all of them if they
// fit together, otherwise the first one with the most power that fits alone. The planner and
// the controller share it so they pick the same units.
func fitUnits[T any](units []T, room float64, power func(T) float64) []T {
	var total float64
	for _, u := range units {
		total += power(u)
	}
	if total <= room {
		return units
	}

	best := -1
	for i, u := range units {
		if p := power(u); p <= room && (best < 0 || p > power(units[best])) {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	return units[best : best+1]
}
