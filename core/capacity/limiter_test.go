package capacity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

const (
	minPower = 1380.0 // 6 A single phase
	maxPower = 7360.0 // 32 A single phase
)

// settings without cooking windows, so tests do not depend on the time of day
func noCooking() Settings {
	s := DefaultSettings()
	s.Cooking = Windows{}
	return s
}

// at10 is a budget 10 minutes into a weekday evening slot (outside cooking windows)
func at10(s Settings, energy float64) (Budget, time.Time) {
	now := time.Date(2026, 11, 3, 21, 10, 0, 0, time.UTC)
	return Compute(s, Measurement{Time: now, SlotEnergy: energy}), now
}

func TestLimiterCeiling(t *testing.T) {
	s := noCooking()
	b, now := at10(s, 0) // rest capped at 2350 W

	tc := []struct {
		name        string
		grid, power float64
		want        float64
		limited     bool
	}{
		{"budget minus house", 300, 0, 2050, true},
		{"own power counts back", 2000, 1700, 2050, true},
		{"solar surplus lifts the ceiling to maximum", -5000, 2000, maxPower, false},
		{"solar surplus partially", -2000, 1380, 5730, true},
	}

	for _, c := range tc {
		t.Run(c.name, func(t *testing.T) {
			l := NewLimiter(s)
			d := l.Limit(b, c.grid, LoadpointState{Now: now, Mode: ModeSmart, Connected: true, ChargePower: c.power, MinPower: minPower, MaxPower: maxPower})
			assert.InDelta(t, c.want, d.MaxPower, 0.5)
			assert.Equal(t, c.limited, d.Limited)
			if c.limited {
				assert.Equal(t, ReasonBudget, d.Reason)
			}
		})
	}
}

func TestLimiterNotConnected(t *testing.T) {
	s := noCooking()
	b, now := at10(s, 0.6)

	l := NewLimiter(s)
	l.paused = true

	d := l.Limit(b, 3000, LoadpointState{Now: now, Mode: ModeFast, Connected: false, MinPower: minPower, MaxPower: maxPower})
	assert.Equal(t, maxPower, d.MaxPower, "no limit without vehicle")
	assert.False(t, d.Limited)
	assert.False(t, l.Paused(), "disconnect resets the pause")
}

func TestLimiterMinCurrentHold(t *testing.T) {
	s := noCooking()
	st := func(now time.Time) LoadpointState {
		return LoadpointState{Now: now, Mode: ModeFast, Connected: true, ChargePower: minPower, MinPower: minPower, MaxPower: maxPower}
	}

	t.Run("hold at minimum while the slot ends below target", func(t *testing.T) {
		// 0.35 kWh after 10 minutes, house 1120 W plus car 1380 W
		b, now := at10(s, 0.35)
		l := NewLimiter(s)
		d := l.Limit(b, 2500, st(now))
		assert.Equal(t, minPower, d.MaxPower)
		assert.Equal(t, ReasonMinCurrentHold, d.Reason)
		assert.False(t, l.Paused())
	})

	t.Run("stop when minimum would exceed the target", func(t *testing.T) {
		b, now := at10(s, 0.40)
		l := NewLimiter(s)
		d := l.Limit(b, 2500, st(now))
		assert.Equal(t, 0.0, d.MaxPower)
		assert.Equal(t, ReasonBudget, d.Reason)
		assert.True(t, l.Paused())
	})

	t.Run("no hold when not charging yet", func(t *testing.T) {
		b, now := at10(s, 0.35)
		l := NewLimiter(s)
		state := st(now)
		state.ChargePower = 0
		d := l.Limit(b, 1700, state) // ceiling 650 W
		assert.Equal(t, 0.0, d.MaxPower)
		assert.True(t, l.Paused())
	})
}

func TestLimiterRestart(t *testing.T) {
	s := noCooking()
	l := NewLimiter(s)

	slot := time.Date(2026, 11, 3, 21, 0, 0, 0, time.UTC)
	state := func(now time.Time) LoadpointState {
		return LoadpointState{Now: now, Mode: ModeFast, Connected: true, MinPower: minPower, MaxPower: maxPower}
	}
	// fresh budget each call: rest 2350 W, house load decides the headroom
	limit := func(d time.Duration, house float64) Decision {
		now := slot.Add(d)
		b := Compute(s, Measurement{Time: now, MonthPeak: 0})
		return l.Limit(b, house, state(now))
	}

	// stop: house 2000 W leaves 350 W
	d := limit(0, 2000)
	assert.Equal(t, 0.0, d.MaxPower)
	assert.True(t, l.Paused())

	// headroom 2350 - 300 = 2050 W >= 1380 + 300, but the pause is too short
	for _, at := range []time.Duration{time.Minute, 2 * time.Minute, 3*time.Minute + 30*time.Second} {
		d = limit(at, 300)
		assert.Equal(t, ReasonPaused, d.Reason, "at %v", at)
		assert.Equal(t, 0.0, d.MaxPower)
	}

	// headroom drops below minimum + 300 W: the 120 s hold starts over
	d = limit(4*time.Minute, 700) // 1650 W < 1680 W
	assert.Equal(t, ReasonPaused, d.Reason)

	// 5 minutes paused, but headroom only since 4:30
	d = limit(4*time.Minute+30*time.Second, 300)
	assert.Equal(t, ReasonPaused, d.Reason)
	d = limit(6*time.Minute, 300)
	assert.Equal(t, ReasonPaused, d.Reason, "headroom for 90 s only")

	// 120 s of headroom and more than 5 minutes paused
	d = limit(6*time.Minute+30*time.Second, 300)
	assert.False(t, l.Paused())
	assert.InDelta(t, 2050, d.MaxPower, 0.5)
	assert.Equal(t, ReasonBudget, d.Reason)
}

func TestLimiterCookingWindow(t *testing.T) {
	loc := brussels(t)
	s := DefaultSettings()

	weekday := time.Date(2026, 11, 3, 17, 30, 0, 0, loc) // Tuesday
	saturday := time.Date(2026, 11, 7, 11, 0, 0, 0, loc)
	outside := time.Date(2026, 11, 3, 20, 0, 0, 0, loc)

	budget := func(now time.Time) Budget {
		return Compute(s, Measurement{Time: now})
	}

	tc := []struct {
		name    string
		now     time.Time
		mode    Mode
		grid    float64
		power   float64
		battery float64
		want    float64
		reason  Reason
		cooking bool
	}{
		{"fast at minimum on weekday evening", weekday, ModeFast, 1680, 1380, 0, minPower, ReasonCooking, true},
		{"fast at minimum even with full battery", weekday, ModeFast, 1680, 1380, 3000, minPower, ReasonCooking, true},
		{"fast at minimum on saturday morning", saturday, ModeFast, 1680, 1380, 0, minPower, ReasonCooking, true},
		{"fast unrestricted outside the window", outside, ModeFast, 1680, 1380, 0, 2050, ReasonBudget, false},
		{"smart on surplus only without battery", weekday, ModeSmart, -400, 1380, 500, 1780, ReasonCooking, true},
		{"smart with battery uses the budget", weekday, ModeSmart, 1680, 1380, 2500, 2050, ReasonBudget, true},
		{"smart without surplus and battery stops", weekday, ModeSmart, 500, 0, 0, 0, ReasonCooking, true},
	}

	for _, c := range tc {
		t.Run(c.name, func(t *testing.T) {
			l := NewLimiter(s)
			d := l.Limit(budget(c.now), c.grid, LoadpointState{
				Now: c.now, Mode: c.mode, Connected: true,
				ChargePower: c.power, MinPower: minPower, MaxPower: maxPower,
				BatteryAvailable: c.battery,
			})
			assert.InDelta(t, c.want, d.MaxPower, 0.5)
			assert.Equal(t, c.reason, d.Reason)
			assert.Equal(t, c.cooking, d.Cooking)
		})
	}
}

func TestLimiterCookingNoGridHold(t *testing.T) {
	loc := brussels(t)
	s := DefaultSettings()
	now := time.Date(2026, 11, 3, 17, 40, 0, 0, loc)

	// smart charging at minimum, surplus gone: no hold from the grid in the cooking window
	b := Compute(s, Measurement{Time: now, SlotEnergy: 0.1})
	l := NewLimiter(s)
	d := l.Limit(b, 800, LoadpointState{Now: now, Mode: ModeSmart, Connected: true, ChargePower: minPower, MinPower: minPower, MaxPower: maxPower})
	assert.Equal(t, 0.0, d.MaxPower)
	assert.True(t, l.Paused())
}

func TestFallback(t *testing.T) {
	d := Fallback(LoadpointState{Mode: ModeFast, Connected: true, MinPower: minPower, MaxPower: maxPower})
	assert.Equal(t, minPower, d.MaxPower)
	assert.Equal(t, ReasonMeter, d.Reason)

	d = Fallback(LoadpointState{Mode: ModeOff, Connected: true, MinPower: minPower, MaxPower: maxPower})
	assert.False(t, d.Limited)
}
