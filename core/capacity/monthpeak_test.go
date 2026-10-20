package capacity

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMonthPeakFromMeter(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Brussels")
	require.NoError(t, err)

	now := time.Date(2026, 10, 20, 9, 0, 0, 0, loc)
	at := func(month time.Month, day, h, m, s int) time.Time {
		return time.Date(2026, month, day, h, m, s, 0, loc)
	}

	for _, tc := range []struct {
		name       string
		power      float64
		registered time.Time
		now        time.Time
		peak       float64
		start      time.Time
		err        bool
	}{
		{"registered at slot end", 3428, at(10, 18, 12, 0, 0), now, 3428, at(10, 18, 11, 45, 0), false},
		{"registered shortly after slot end", 3428, at(10, 18, 12, 0, 40), now, 3428, at(10, 18, 11, 45, 0), false},
		{"no registration", 0, time.Time{}, now, 0, time.Time{}, false},
		{"power without timestamp", 4200, time.Time{}, now, 0, time.Time{}, true},
		{"previous month", 4000, at(9, 30, 18, 0, 0), now, 0, time.Time{}, false},
		{"last slot of previous month", 4000, at(10, 1, 0, 0, 5), now, 0, time.Time{}, false},
		{"first slot of the month", 3000, at(10, 1, 0, 15, 0), now, 3000, at(10, 1, 0, 0, 0), false},
		{"clock slightly ahead", 3000, now.Add(time.Minute), now, 3000, at(10, 20, 8, 45, 0), false},
		{"future", 3000, now.Add(3 * time.Minute), now, 0, time.Time{}, true},
		{"negative", -1, at(10, 18, 12, 0, 0), now, 0, time.Time{}, true},
		{"too high", 50001, at(10, 18, 12, 0, 0), now, 0, time.Time{}, true},
		{"NaN", math.NaN(), at(10, 18, 12, 0, 0), now, 0, time.Time{}, true},
		{"Inf", math.Inf(1), at(10, 18, 12, 0, 0), now, 0, time.Time{}, true},
		{"winter time", 2800, at(10, 25, 3, 0, 0), at(10, 26, 9, 0, 0), 2800, at(10, 25, 2, 45, 0), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := MonthPeakFromMeter(tc.power, tc.registered, tc.now, loc)
			if tc.err {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.peak, p.Power)
			assert.True(t, tc.start.Equal(p.Start), "start %v, want %v", p.Start, tc.start)
		})
	}
}

func TestSlotPower(t *testing.T) {
	assert.InDelta(t, 3428.0, SlotPower(0.857), 1e-9)
}

func TestMonthPeakTracker(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Brussels")
	require.NoError(t, err)

	base := time.Date(2026, 10, 20, 9, 5, 0, 0, loc) // 5 minutes into a slot
	evcc := Peak{3384, time.Date(2026, 10, 18, 11, 45, 0, 0, loc)}
	meter := Peak{3450, time.Date(2026, 10, 18, 11, 45, 0, 0, loc)}

	t.Run("own count without meter", func(t *testing.T) {
		var tr MonthPeakTracker
		tr.SetEvcc(evcc)

		s := tr.Update(base, loc)
		assert.Equal(t, sourceEvcc, s.Source)
		assert.Equal(t, evcc, s.Peak)
		assert.True(t, s.Switched, "first update reports the source")
		assert.False(t, s.Mismatch)

		assert.False(t, tr.Update(base.Add(time.Minute), loc).Switched)
	})

	t.Run("meter preferred, mismatch once", func(t *testing.T) {
		var tr MonthPeakTracker
		tr.SetEvcc(evcc)
		tr.SetMeter(meter, base)

		s := tr.Update(base, loc)
		assert.Equal(t, sourceMeter, s.Source)
		assert.Equal(t, meter, s.Peak)
		assert.Equal(t, evcc, s.Evcc)
		assert.True(t, s.Mismatch)

		tr.SetMeter(meter, base.Add(30*time.Second))
		assert.False(t, tr.Update(base.Add(30*time.Second), loc).Mismatch, "same pair")

		tr.SetMeter(Peak{3500, meter.Start}, base.Add(time.Minute))
		assert.True(t, tr.Update(base.Add(time.Minute), loc).Mismatch, "new pair")
	})

	t.Run("no mismatch within 50 W", func(t *testing.T) {
		// 18 October 2026: meter 3428 W, own count 3384 W
		var tr MonthPeakTracker
		tr.SetEvcc(evcc)
		tr.SetMeter(Peak{3428, meter.Start}, base)
		s := tr.Update(base, loc)
		assert.Equal(t, 3428.0, s.Power)
		assert.False(t, s.Mismatch)
	})

	t.Run("no mismatch right after slot boundary", func(t *testing.T) {
		now := time.Date(2026, 10, 20, 9, 1, 0, 0, loc)
		var tr MonthPeakTracker
		tr.SetEvcc(evcc)
		tr.SetMeter(meter, now)
		assert.False(t, tr.Update(now, loc).Mismatch)
		assert.True(t, tr.Update(now.Add(time.Minute), loc).Mismatch)
	})

	t.Run("fallback after 10 minutes", func(t *testing.T) {
		var tr MonthPeakTracker
		tr.SetEvcc(evcc)
		tr.SetMeter(meter, base)
		require.Equal(t, sourceMeter, tr.Update(base, loc).Source)

		s := tr.Update(base.Add(meterPeakMaxAge), loc)
		assert.Equal(t, sourceMeter, s.Source)

		s = tr.Update(base.Add(meterPeakMaxAge+time.Second), loc)
		assert.Equal(t, sourceEvcc, s.Source)
		assert.Equal(t, evcc, s.Peak)
		assert.True(t, s.Switched)
		assert.False(t, s.Mismatch)

		tr.SetMeter(meter, base.Add(meterPeakMaxAge+time.Minute))
		s = tr.Update(base.Add(meterPeakMaxAge+time.Minute), loc)
		assert.Equal(t, sourceMeter, s.Source)
		assert.True(t, s.Switched)
	})

	t.Run("meter reading from previous month", func(t *testing.T) {
		read := time.Date(2026, 10, 31, 23, 58, 0, 0, loc)
		var tr MonthPeakTracker
		tr.SetEvcc(Peak{})
		tr.SetMeter(meter, read)

		s := tr.Update(read.Add(3*time.Minute), loc)
		assert.Equal(t, sourceEvcc, s.Source)
		assert.Zero(t, s.Power)
	})
}
