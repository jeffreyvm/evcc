package capacity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAttribute(t *testing.T) {
	tc := []struct {
		name                string
		average, vehicle    float64
		exceeded, byVehicle bool
	}{
		{"below target", 2300, 1400, false, false},
		{"within the tolerance", 2440, 1400, false, false},
		{"exceeded by charging", 2600, 1400, true, true},
		{"house alone exceeds", 3300, 600, true, false},
		{"house alone exceeds without charging", 3300, 0, true, false},
		{"house exactly at the limit", 2600, 150, true, true},
	}

	for _, c := range tc {
		t.Run(c.name, func(t *testing.T) {
			exceeded, byVehicle := Attribute(c.average, c.vehicle, 2350)
			assert.Equal(t, c.exceeded, exceeded, "exceeded")
			assert.Equal(t, c.byVehicle, byVehicle, "by vehicle")
		})
	}
}

func TestSlotTracker(t *testing.T) {
	s := DefaultSettings()
	slot := time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC)

	var tr SlotTracker

	// house 1200 W and car 1380 W the whole slot, a cycle every 30 s
	cycle := func(d time.Duration, energy, grid, car float64) (SlotResult, bool) {
		now := slot.Add(d)
		b := Compute(s, Measurement{Time: now, SlotEnergy: energy, GridPower: grid})
		return tr.Update(b, car, now)
	}

	_, done := cycle(5*time.Second, kWh(2580, 5*time.Second), 2580, 1380)
	assert.False(t, done, "first cycle")

	var d time.Duration
	for d = 35 * time.Second; d < SlotDuration; d += 30 * time.Second {
		_, done = cycle(d, kWh(2580, d), 2580, 1380)
		assert.False(t, done, "%v", d)
	}

	res, done := cycle(SlotDuration+5*time.Second, kWh(2580, 5*time.Second), 2580, 1380)
	assert.True(t, done)
	assert.Equal(t, slot, res.Start)
	assert.InDelta(t, 2580, res.Average, 1)
	assert.InDelta(t, 1380, res.Vehicle, 1)
	assert.InDelta(t, 1200, res.House(), 1)
	assert.Equal(t, 2350.0, res.Target)
	assert.True(t, res.Exceeded)
	assert.True(t, res.ByVehicle)
	assert.False(t, res.Partial)
}

func TestSlotTrackerPartial(t *testing.T) {
	s := DefaultSettings()
	slot := time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC)

	update := func(tr *SlotTracker, d time.Duration, energy float64) (SlotResult, bool) {
		now := slot.Add(d)
		b := Compute(s, Measurement{Time: now, SlotEnergy: energy, GridPower: 1000})
		return tr.Update(b, 0, now)
	}

	t.Run("started late in the slot", func(t *testing.T) {
		var tr SlotTracker
		update(&tr, 10*time.Minute, 0.2)
		res, done := update(&tr, SlotDuration+10*time.Second, 0)
		assert.True(t, done)
		assert.True(t, res.Partial)
	})

	t.Run("gap within the slot", func(t *testing.T) {
		var tr SlotTracker
		update(&tr, 10*time.Second, 0)
		update(&tr, 5*time.Minute, 0.08)
		res, done := update(&tr, SlotDuration+10*time.Second, 0)
		assert.True(t, done)
		assert.True(t, res.Partial)
	})

	// cycles every 30 s from 5 s into the slot, counting starts at from
	counted := func(from time.Duration) SlotResult {
		var tr SlotTracker
		for d := 5 * time.Second; d < SlotDuration; d += 30 * time.Second {
			now := slot.Add(d)
			b := Compute(s, Measurement{Time: now, SlotEnergy: kWh(1000, max(0, d-from)), CountedFrom: slot.Add(from), GridPower: 1000})
			tr.Update(b, 0, now)
		}
		now := slot.Add(SlotDuration + 5*time.Second)
		res, done := tr.Update(Compute(s, Measurement{Time: now, GridPower: 1000}), 0, now)
		require.True(t, done)
		return res
	}

	t.Run("first reading a few seconds after the boundary", func(t *testing.T) {
		assert.False(t, counted(5*time.Second).Partial)
	})

	t.Run("counting started minutes late", func(t *testing.T) {
		assert.True(t, counted(3*time.Minute).Partial)
	})

	t.Run("gap over a whole slot reports nothing", func(t *testing.T) {
		var tr SlotTracker
		update(&tr, 10*time.Second, 0)
		_, done := update(&tr, 2*SlotDuration+10*time.Second, 0)
		assert.False(t, done)
	})

	t.Run("reset forgets the slot", func(t *testing.T) {
		var tr SlotTracker
		update(&tr, 10*time.Second, 0)
		tr.Reset()
		_, done := update(&tr, SlotDuration+10*time.Second, 0)
		assert.False(t, done)
	})
}
