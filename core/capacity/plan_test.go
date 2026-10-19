package capacity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestPlanEnergy(t *testing.T) {
	loc := brussels(t)
	s := DefaultSettings()

	constant := func(w float64) func(time.Time) float64 {
		return func(time.Time) float64 { return w }
	}

	tc := []struct {
		name    string
		now, dl time.Time
		home    float64
		want    float64 // kWh
	}{
		{
			name: "overnight at 2.0 kW",
			now:  time.Date(2026, 11, 3, 22, 0, 0, 0, loc), dl: time.Date(2026, 11, 4, 7, 0, 0, 0, loc),
			home: 350, want: 2.0 * 9,
		},
		{
			name: "home too high for minimum current",
			now:  time.Date(2026, 11, 3, 22, 0, 0, 0, loc), dl: time.Date(2026, 11, 4, 7, 0, 0, 0, loc),
			home: 1100, want: 0,
		},
		{
			name: "minimum current in the cooking window",
			now:  time.Date(2026, 11, 3, 16, 0, 0, 0, loc), dl: time.Date(2026, 11, 3, 20, 0, 0, 0, loc),
			home: 350, want: 2.0 + 1.38*2 + 2.0,
		},
		{
			name: "night with the extra hour of winter time",
			now:  time.Date(2026, 10, 24, 22, 0, 0, 0, loc), dl: time.Date(2026, 10, 25, 7, 0, 0, 0, loc),
			home: 350, want: 2.0 * 10,
		},
		{
			name: "deadline passed",
			now:  time.Date(2026, 11, 3, 22, 0, 0, 0, loc), dl: time.Date(2026, 11, 3, 21, 0, 0, 0, loc),
			home: 350, want: 0,
		},
	}

	for _, c := range tc {
		t.Run(c.name, func(t *testing.T) {
			b := Compute(s, Measurement{Time: c.now})
			got := PlanEnergy(s, PlanInput{
				Now: c.now, Deadline: c.dl, Budget: b,
				HomePower: constant(c.home), MinPower: minPower, MaxPower: maxPower,
			})
			assert.InDelta(t, c.want, got, 1e-6)
		})
	}
}

func TestPlanEnergyCurrentSlot(t *testing.T) {
	loc := brussels(t)
	s := noCooking()

	// 10 minutes into the slot with 0.5 kWh used: 87.5 Wh left in 5 minutes = 1050 W
	now := time.Date(2026, 11, 3, 22, 10, 0, 0, loc)
	b := Compute(s, Measurement{Time: now, SlotEnergy: 0.5})

	got := PlanEnergy(s, PlanInput{
		Now: now, Deadline: now.Add(5*time.Minute + time.Hour), Budget: b,
		HomePower: func(time.Time) float64 { return 350 }, MinPower: minPower, MaxPower: maxPower,
	})

	// the current slot is below minimum power, the next hour gets 2.0 kW
	assert.InDelta(t, 2.0, got, 1e-6)
}
