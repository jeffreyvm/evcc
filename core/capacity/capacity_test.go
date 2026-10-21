package capacity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func brussels(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Brussels")
	require.NoError(t, err)
	return loc
}

func TestTarget(t *testing.T) {
	s := DefaultSettings()

	assert.Equal(t, 2350.0, Target(s, 0), "floor applies without month peak")
	assert.Equal(t, 2350.0, Target(s, 1800), "floor applies below floor")
	assert.Equal(t, 3280.0, Target(s, 3430), "month peak applies above floor")
}

func TestMonthStart(t *testing.T) {
	loc := brussels(t)

	tc := []struct {
		at, want time.Time
	}{
		{time.Date(2026, 10, 31, 23, 59, 0, 0, loc), time.Date(2026, 10, 1, 0, 0, 0, 0, loc)},
		{time.Date(2026, 11, 1, 0, 0, 0, 0, loc), time.Date(2026, 11, 1, 0, 0, 0, 0, loc)},
		// 23:30 UTC on 31 October is already November in Brussels
		{time.Date(2026, 10, 31, 23, 30, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, loc)},
		{time.Date(2027, 1, 1, 0, 10, 0, 0, loc), time.Date(2027, 1, 1, 0, 0, 0, 0, loc)},
	}

	for _, c := range tc {
		assert.True(t, c.want.Equal(MonthStart(c.at, loc)), "%v: got %v, want %v", c.at, MonthStart(c.at, loc), c.want)
	}
}

func TestSlotStartAcrossDST(t *testing.T) {
	loc := brussels(t)

	// 25 October 2026: 02:00-03:00 local happens twice
	first := time.Date(2026, 10, 25, 0, 20, 0, 0, time.UTC) // 02:20 CEST
	second := first.Add(time.Hour)                          // 02:20 CET

	assert.Equal(t, first.In(loc).Hour(), second.In(loc).Hour(), "same wall clock hour")
	assert.NotEqual(t, SlotStart(first), SlotStart(second), "distinct slots")

	for _, at := range []time.Time{first, second} {
		s := SlotStart(at).In(loc)
		assert.Equal(t, 15, s.Minute(), "slot boundary on the local quarter hour")
		assert.Equal(t, 0, s.Second())
	}

	// 29 March 2026: 02:00-03:00 local does not exist, slots stay 15 minutes apart
	before := time.Date(2026, 3, 29, 0, 45, 0, 0, time.UTC) // 01:45 CET
	after := SlotStart(before).Add(SlotDuration)
	assert.Equal(t, 3, after.In(loc).Hour(), "01:45 CET is followed by 03:00 CEST")
}

func TestCompute(t *testing.T) {
	s := DefaultSettings()
	start := time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return start.Add(d) }

	tc := []struct {
		name       string
		m          Measurement
		rest       float64
		energy     float64
		projection float64
		exceeded   bool
	}{
		{
			name: "empty slot gets the target",
			m:    Measurement{Time: start},
			rest: 2350, energy: 0, projection: 0,
		},
		{
			name: "half slot at target keeps the target",
			m:    Measurement{Time: at(7*time.Minute + 30*time.Second), SlotEnergy: 0.29375, GridPower: 2350},
			rest: 2350, energy: 0.29375, projection: 0.5875,
		},
		{
			name: "overconsumed first half leaves less",
			m:    Measurement{Time: at(7*time.Minute + 30*time.Second), SlotEnergy: 0.5, GridPower: 4000},
			// 0.0875 kWh left, of which 4 kW x 15 s dead time = 0.0167 kWh, over 435 s
			rest: 586.2, energy: 0.5, projection: 1.0, exceeded: true,
		},
		{
			name: "exhausted budget allows nothing",
			m:    Measurement{Time: at(10 * time.Minute), SlotEnergy: 0.6, GridPower: 500},
			rest: 0, energy: 0.6, projection: 0.6 + 0.5*5.0/60, exceeded: true,
		},
		{
			name: "almost empty slot near its end is capped at target",
			m:    Measurement{Time: at(14*time.Minute + 59*time.Second), SlotEnergy: 0.1, GridPower: 0},
			rest: 2350, energy: 0.1, projection: 0.1,
		},
		{
			name: "export does not reduce the projection",
			m:    Measurement{Time: at(5 * time.Minute), SlotEnergy: 0.05, GridPower: -3000},
			rest: 2350, energy: 0.05, projection: 0.05,
		},
		{
			name: "restart mid slot assumes import at target until counting started",
			m:    Measurement{Time: at(10 * time.Minute), CountedFrom: at(5 * time.Minute), SlotEnergy: 0.1, GridPower: 1000},
			// assumed 2350 W for 5 minutes = 0.195833 kWh
			rest: 2350, energy: 0.1 + 2350.0*5/60/1e3, projection: 0.1 + 2350.0*5/60/1e3 + 1.0*5/60,
		},
		{
			name: "month peak above floor raises the target",
			m:    Measurement{Time: at(5 * time.Minute), SlotEnergy: 0.25, GridPower: 3000, MonthPeak: 3430},
			// target 3280 W, allowed 0.82 kWh, 0.57 kWh left in 10 minutes = 3420 W, capped
			rest: 3280, energy: 0.25, projection: 0.75,
		},
	}

	for _, c := range tc {
		t.Run(c.name, func(t *testing.T) {
			b := Compute(s, c.m)
			assert.InDelta(t, c.rest, b.Rest, 0.5, "rest")
			assert.InDelta(t, c.energy, b.Energy, 1e-6, "energy")
			assert.InDelta(t, c.projection, b.Projection, 1e-6, "projection")
			assert.Equal(t, c.exceeded, b.Exceeded(), "exceeded")
			assert.Equal(t, start, b.SlotStart)
		})
	}
}

func TestComputeDeadTime(t *testing.T) {
	start := time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC)
	m := Measurement{Time: start.Add(11 * time.Minute), SlotEnergy: 0.45, GridPower: 3000}

	none := DefaultSettings()
	none.DeadTime = 0
	assert.InDelta(t, 2062.5, Compute(none, m).Rest, 0.5, "0.1375 kWh over 4 minutes")

	// 3 kW continue for 15 s before a new ceiling shows: 0.0125 kWh less over 225 s
	assert.InDelta(t, 2000, Compute(DefaultSettings(), m).Rest, 0.5)

	// near the end the dead time leaves nothing to compensate over
	m.Time = start.Add(14*time.Minute + 40*time.Second)
	m.SlotEnergy = 0.57
	assert.InDelta(t, 2350, Compute(DefaultSettings(), m).Rest, 0.5, "0.0175 kWh over 20 s, capped at target")
}

func TestHoldsAt(t *testing.T) {
	s := DefaultSettings()
	start := time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC)
	b := Compute(s, Measurement{Time: start.Add(10 * time.Minute), SlotEnergy: 0.35})

	assert.True(t, b.HoldsAt(2000, 0), "0.35 + 2.0 kW x 5 min = 0.517 kWh <= 0.5875 kWh")
	assert.False(t, b.HoldsAt(3000, 0), "0.35 + 3.0 kW x 5 min = 0.600 kWh > 0.5875 kWh")
	assert.True(t, b.HoldsAt(3000, -1000), "delta applies to grid power")
	assert.True(t, b.HoldsAt(-2000, 0), "export counts as zero import")
}

func TestPeakWatch(t *testing.T) {
	s := DefaultSettings()
	start := time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC)
	w := NewPeakWatch(s.ThreatDelay)

	// 4 kW from 2 minutes on: projection exceeds the target
	measure := func(d time.Duration, grid float64) (Budget, time.Time) {
		now := start.Add(d)
		energy := 4.0 * (d - 2*time.Minute).Hours()
		return Compute(s, Measurement{Time: now, SlotEnergy: max(0, energy), GridPower: grid}), now
	}

	b, now := measure(2*time.Minute, 4000)
	assert.False(t, w.Update(b, 4000, now), "first cycle above budget")

	b, now = measure(2*time.Minute+29*time.Second, 4000)
	assert.False(t, w.Update(b, 4000, now), "29 s is a spike")

	b, now = measure(2*time.Minute+30*time.Second, 4000)
	assert.True(t, w.Update(b, 4000, now), "30 s above budget with projection above target")

	b, now = measure(3*time.Minute, 1000)
	assert.False(t, w.Update(b, 1000, now), "below budget clears the threat")

	b, now = measure(3*time.Minute+10*time.Second, 4000)
	assert.False(t, w.Update(b, 4000, now), "debounce starts over")
}

func TestPeakWatchNewSlot(t *testing.T) {
	s := DefaultSettings()
	w := NewPeakWatch(s.ThreatDelay)
	start := time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC)

	// above budget at the very end of a slot
	end := start.Add(14*time.Minute + 50*time.Second)
	b := Compute(s, Measurement{Time: end, SlotEnergy: 0.7, GridPower: 5000})
	assert.False(t, w.Update(b, 5000, end))

	// 20 s later in the next slot: the debounce must not carry over
	next := end.Add(20 * time.Second)
	b = Compute(s, Measurement{Time: next, SlotEnergy: 0.01, GridPower: 5000})
	assert.False(t, w.Update(b, 5000, next.Add(15*time.Second)))
}

func TestPeakWatchTimeZone(t *testing.T) {
	s := DefaultSettings()
	w := NewPeakWatch(s.ThreatDelay)
	loc := brussels(t)
	start := time.Date(2026, 11, 3, 18, 2, 0, 0, time.UTC)

	// budget in UTC, clock in local time: the same slot, so the debounce carries on
	update := func(d time.Duration) bool {
		now := start.Add(d)
		b := Compute(s, Measurement{Time: now, SlotEnergy: 4.0 * d.Hours(), GridPower: 4000})
		return w.Update(b, 4000, now.In(loc))
	}

	assert.False(t, update(0))
	assert.True(t, update(30*time.Second))
}
