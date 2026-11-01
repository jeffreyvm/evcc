package capacity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reserveDay builds per slot series from now until two days later: the house at 250W at night
// (p90) until 07:00, 400W during the day, 2700W from 17:00 to 21:00, 250W after; the median is
// 100W lower. Solar shines with sun W from 10:00 to 15:00.
func reserveDay(now time.Time, sun float64) (p90, p50, solar []float64) {
	for slot := SlotStart(now); slot.Before(now.Add(48 * time.Hour)); slot = slot.Add(SlotDuration) {
		h := slot.In(Location).Hour()
		house := 250.0
		switch {
		case h >= 7 && h < 17:
			house = 400
		case h >= 17 && h < 21:
			house = 2700
		}
		p90 = append(p90, house)
		p50 = append(p50, house-100)

		var pv float64
		if h >= 10 && h < 15 {
			pv = sun
		}
		solar = append(solar, pv)
	}
	return p90, p50, solar
}

// both units at 6%, 0.6 kWh, with a 5% minimum
func reserveUnits(soc float64) BatteryUnits {
	g, z := garage(soc, 0), zolder(soc, 0)
	g.MinSoc, z.MinSoc = 5, 5
	return BatteryUnits{g, z}
}

func reserveInput(now time.Time, sun float64, soc float64) ReserveInput {
	p90, p50, solar := reserveDay(now, sun)
	return ReserveInput{
		Now:         now,
		Units:       reserveUnits(soc),
		Target:      2350,
		HouseP90:    p90,
		HouseP50:    p50,
		ProfileDays: 28,
		Solar:       solar,
		SolarScale:  1,
	}
}

// The example of the design, at 01:00 in November:
//
//	season floor 30% of 10 kWh               3.0 kWh
//	evening peaks 16 × 350W × 0.25h / 0.95    1.47 kWh, plus the 1 kWh buffer: 2.47, the floor wins
//	reserve incl. the 5% minimum              3.5 kWh
//	stored                                    0.6 kWh
//	solar 20 slots × (600 − 300)W × 0.25h × 0.95  1.425 kWh
//	need                                      1.475 kWh
//
// Per unit: garage 1.4 − 0.24 − 0.57 = 0.59 kWh, 0.19 kWh per slot → 4 slots; zolder
// 2.1 − 0.36 − 0.855 = 0.885 kWh, 0.285 kWh per slot → 4 slots. The night leaves
// 2350 − 250 = 2100W, enough for both: the four latest night slots, 06:00 to 07:00.
func TestReservePlanNovemberNight(t *testing.T) {
	now := time.Date(2026, 11, 3, 1, 0, 0, 0, Location)
	s := DefaultReserveSettings()

	p := s.PlanGridCharge(reserveInput(now, 600, 6))

	assert.Equal(t, time.Date(2026, 11, 3, 17, 0, 0, 0, Location), p.Deadline)
	assert.InDelta(t, 3.0, p.Season, 1e-9)
	assert.InDelta(t, 16*350*0.25/1e3/0.95+1, p.Peaks, 1e-9)
	assert.InDelta(t, 3.5, p.Reserve, 1e-9)
	assert.InDelta(t, 0.6, p.Energy, 1e-9)
	assert.InDelta(t, 1.425, p.Surplus, 1e-9)
	assert.InDelta(t, 1.475, p.Need, 1e-9)
	assert.False(t, p.Fixed)
	assert.Zero(t, p.Shortage)

	require.Len(t, p.Slots, 4)
	for i, slot := range p.Slots {
		assert.Equal(t, time.Date(2026, 11, 3, 6, 15*i, 0, 0, Location), slot.Start.In(Location), "slot %d", i)
		assert.ElementsMatch(t, []string{"garage", "zolder"}, slot.Units)
		assert.InDelta(t, 2000, slot.Power, 1e-9)
	}

	assert.Equal(t, map[string]bool{"garage": true, "zolder": true}, p.Units(time.Date(2026, 11, 3, 6, 20, 0, 0, Location)))
	assert.Nil(t, p.Units(now))
}

// In the day the house leaves 1950W: too little for both, so they take turns, the zolder first
func TestReservePlanDayZolderOnly(t *testing.T) {
	now := time.Date(2026, 11, 3, 7, 0, 0, 0, Location)
	s := DefaultReserveSettings()

	p := s.PlanGridCharge(reserveInput(now, 0, 6))
	assert.InDelta(t, 2.9, p.Need, 1e-9, "no sun: 3.5 − 0.6")
	require.NotEmpty(t, p.Slots)

	var zolder, garage int
	for _, slot := range p.Slots {
		require.Len(t, slot.Units, 1, "%s", slot.Start)
		switch slot.Units[0] {
		case "zolder":
			assert.InDelta(t, 1200, slot.Power, 1e-9)
			zolder++
		case "garage":
			assert.InDelta(t, 800, slot.Power, 1e-9)
			garage++
		}
	}
	// zolder needs 2.1 − 0.36 = 1.74 kWh: 7 slots of 0.285 kWh, garage 1.4 − 0.24 = 1.16 kWh: 7 slots of 0.19 kWh
	assert.Equal(t, 7, zolder)
	assert.Equal(t, 7, garage)
	assert.Zero(t, p.Shortage)

	// the latest slots before the deadline, the zolder in the last ones
	last := p.Slots[len(p.Slots)-1]
	assert.Equal(t, time.Date(2026, 11, 3, 16, 45, 0, 0, Location), last.Start.In(Location))
	assert.Equal(t, []string{"zolder"}, last.Units)

	// a slot without room for either unit leaves a shortage
	in := reserveInput(now, 0, 6)
	for i := range in.HouseP90 {
		in.HouseP90[i] = 1700 // 650W room, less than either unit
	}
	p = DefaultReserveSettings().PlanGridCharge(in)
	assert.Empty(t, p.Slots)
	assert.InDelta(t, 2.9, p.Shortage, 1e-9)
}

// A sunny day covers the reserve: no grid charging
func TestReservePlanSunny(t *testing.T) {
	now := time.Date(2026, 11, 3, 1, 0, 0, 0, Location)
	p := DefaultReserveSettings().PlanGridCharge(reserveInput(now, 2500, 6))
	assert.Zero(t, p.Need)
	assert.Empty(t, p.Slots)
}

// Without forecast the surplus is 0: a few cents of grid charging rather than an empty battery
func TestReservePlanNoForecast(t *testing.T) {
	now := time.Date(2026, 11, 3, 1, 0, 0, 0, Location)
	in := reserveInput(now, 2500, 6)
	in.Solar = nil
	p := DefaultReserveSettings().PlanGridCharge(in)
	assert.Zero(t, p.Surplus)
	assert.InDelta(t, 2.9, p.Need, 1e-9)
}

// With the fixed mode or less than a week of history only the season floor counts
func TestReserveTargetFixed(t *testing.T) {
	now := time.Date(2026, 11, 3, 1, 0, 0, 0, Location)
	s := DefaultReserveSettings()

	in := reserveInput(now, 0, 6)
	// evening peaks of 3.5 kW: 16 × 1150W × 0.25h / 0.95 + 1 = 5.84 kWh, above the floor
	for i := range in.HouseP90 {
		if in.HouseP90[i] == 2700 {
			in.HouseP90[i] = 3500
		}
	}
	r, _, peaks, fixed := s.ReserveTarget(in, s.deadline(in))
	assert.False(t, fixed)
	assert.InDelta(t, 16*1150*0.25/1e3/0.95+1, peaks, 1e-9)
	assert.InDelta(t, peaks+0.5, r, 1e-9)

	in.ProfileDays = 6
	r, _, _, fixed = s.ReserveTarget(in, s.deadline(in))
	assert.True(t, fixed)
	assert.InDelta(t, 3.5, r, 1e-9)

	in.ProfileDays = 28
	s.Mode = ReserveFixed
	r, _, _, fixed = s.ReserveTarget(in, s.deadline(in))
	assert.True(t, fixed)
	assert.InDelta(t, 3.5, r, 1e-9)

	// summer floor 0%: dynamic keeps the buffer
	now = time.Date(2026, 7, 3, 1, 0, 0, 0, Location)
	in = reserveInput(now, 0, 6)
	s = DefaultReserveSettings()
	r, season, _, _ := s.ReserveTarget(in, s.deadline(in))
	assert.Zero(t, season)
	assert.InDelta(t, 16*350*0.25/1e3/0.95+1+0.5, r, 1e-9)

	// never above the capacity
	in.Units = reserveUnits(6)
	for i := range in.HouseP90 {
		in.HouseP90[i] = 9000
	}
	r, _, _, _ = s.ReserveTarget(in, time.Date(2026, 7, 3, 17, 0, 0, 0, Location))
	assert.InDelta(t, 10, r, 1e-9)
}

func TestReserveDeadline(t *testing.T) {
	s := DefaultReserveSettings()

	// after 17:00 the deadline is the next day
	now := time.Date(2026, 11, 3, 21, 30, 0, 0, Location)
	in := reserveInput(now, 0, 6)
	assert.Equal(t, time.Date(2026, 11, 4, 17, 0, 0, 0, Location), s.deadline(in))

	// house above target at 08:00: the reserve is needed then
	now = time.Date(2026, 11, 3, 1, 0, 0, 0, Location)
	in = reserveInput(now, 0, 6)
	in.HouseP90[7*4] = 2600
	assert.Equal(t, time.Date(2026, 11, 3, 8, 0, 0, 0, Location), s.deadline(in))

	// in the evening peak itself the reserve is needed now: no slot left to plan
	now = time.Date(2026, 11, 3, 17, 40, 0, 0, Location)
	in = reserveInput(now, 0, 6)
	assert.Equal(t, time.Date(2026, 11, 3, 17, 30, 0, 0, Location), s.deadline(in))
	assert.Empty(t, s.PlanGridCharge(in).Slots)
}

// The current slot is no candidate while a vehicle charges, nor are slots with solar
func TestReservePlanCandidates(t *testing.T) {
	now := time.Date(2026, 11, 3, 6, 50, 0, 0, Location)
	in := reserveInput(now, 600, 6)
	in.VehicleCharging = true

	p := DefaultReserveSettings().PlanGridCharge(in)
	for _, slot := range p.Slots {
		assert.False(t, slot.Start.Equal(SlotStart(now)), "current slot while charging")
		h := slot.Start.In(Location).Hour()
		assert.False(t, h >= 10 && h < 15, "solar slot %s", slot.Start)
	}
}

// Across the switch to winter time the day has 100 slots: deadlines and peak sums stay on the wall clock
func TestReserveDST(t *testing.T) {
	now := time.Date(2026, 10, 25, 1, 0, 0, 0, Location)
	s := DefaultReserveSettings()
	in := reserveInput(now, 0, 6)
	dl := s.deadline(in)
	assert.Equal(t, 17, dl.In(Location).Hour())
	assert.Equal(t, 25, dl.In(Location).Day())

	// 17:00 to 21:00 is 16 slots also on that day
	_, _, peaks, _ := s.ReserveTarget(in, dl)
	assert.InDelta(t, 16*350*0.25/1e3/0.95+1, peaks, 1e-9)
}

// At 22:00 on 31 October the reserve is for 1 November: November's floor of 30%, not 15%
func TestReserveSeasonOfDeadline(t *testing.T) {
	now := time.Date(2026, 10, 31, 22, 0, 0, 0, Location)
	s := DefaultReserveSettings()
	s.Mode = ReserveFixed
	in := reserveInput(now, 0, 6)
	r, season, _, _ := s.ReserveTarget(in, s.deadline(in))
	assert.InDelta(t, 3.0, season, 1e-9)
	assert.InDelta(t, 3.5, r, 1e-9)
}

// The current slot charges only for the part still ahead: 10 minutes at 16:50 give the zolder
// 2/3 of 0.285 kWh
func TestReservePlanPartialSlot(t *testing.T) {
	now := time.Date(2026, 11, 3, 16, 50, 0, 0, Location)
	in := reserveInput(now, 0, 34)
	in.Units[0].MaxSoc = 34 // the garage is at its limit, only the zolder needs energy: 2.1 − 2.04 = 0.06 kWh

	p := DefaultReserveSettings().PlanGridCharge(in)
	require.Len(t, p.Slots, 1)
	assert.Equal(t, []string{"zolder"}, p.Slots[0].Units)
	assert.Zero(t, p.Shortage, "0.19 kWh in the 10 minutes left cover 0.06 kWh")

	// two thirds of a slot: 1.2 kW × 10 min × 0.95 = 0.19 kWh of the 0.6 kWh the zolder needs
	in.Units[1].Soc = 25 // 2.1 − 1.5 = 0.6 kWh
	p = DefaultReserveSettings().PlanGridCharge(in)
	assert.InDelta(t, 0.6-1.2*10.0/60*0.95, p.Shortage, 1e-9)
}
