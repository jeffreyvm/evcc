package capacity

import (
	"math"
	"math/rand"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The installation of the design: garage 4 kWh / 800 W, zolder 6 kWh / 1200 W, minimum 10%.
// A reserve target of 3.5 kWh splits by capacity: garage 1.4 kWh (35%), zolder 2.1 kWh (35%).

var batStart = time.Date(2026, 11, 3, 20, 0, 0, 0, Location) // Tuesday evening, after the cooking window

func garage(soc, power float64) BatteryUnit {
	return BatteryUnit{Name: "garage", Reachable: true, Soc: soc, MinSoc: 10, MaxSoc: 100, Capacity: 4, Power: power, ChargePower: 800, DischargePower: 800}
}

func zolder(soc, power float64) BatteryUnit {
	return BatteryUnit{Name: "zolder", Reachable: true, Soc: soc, MinSoc: 10, MaxSoc: 100, Capacity: 6, Power: power, ChargePower: 1200, DischargePower: 1200}
}

// batInput is a measured cycle early in a slot: budget 2350W, grid 500W
func batInput(now time.Time, units ...BatteryUnit) BatteryInput {
	return BatteryInput{
		Now:       now,
		Units:     units,
		Measured:  true,
		Budget:    Budget{Target: 2350, SlotStart: SlotStart(now), Remaining: 14 * time.Minute, Rest: 2350},
		GridPower: 500,
		Reserve:   3.5,
	}
}

func modes(d BatteryDecision) map[string]BatteryMode {
	res := make(map[string]BatteryMode)
	for _, u := range d.Units {
		res[u.Name] = u.Mode
	}
	return res
}

func rules(d BatteryDecision) map[string]BatteryRule {
	res := make(map[string]BatteryRule)
	for _, u := range d.Units {
		res[u.Name] = u.Rule
	}
	return res
}

func TestBatteryUnitsShare(t *testing.T) {
	uu := BatteryUnits{garage(50, 0), zolder(50, 0)}
	assert.InDelta(t, 10.0, uu.Capacity(), 1e-9)
	assert.InDelta(t, 5.0, uu.Energy(), 1e-9)
	assert.InDelta(t, 1.0, uu.MinEnergy(), 1e-9)
	assert.InDelta(t, 1.4, uu.share(uu[0], 3.5), 1e-9)
	assert.InDelta(t, 2.1, uu.share(uu[1], 3.5), 1e-9)
	assert.InDelta(t, 0.4, uu.share(uu[0], 0), 1e-9, "never below the unit minimum")

	// a unit not read this cycle keeps its share: the reserve does not jump to the other
	uu[0].Reachable = false
	assert.InDelta(t, 2.1, uu.share(uu[1], 3.5), 1e-9)
	assert.InDelta(t, 1.4, uu.share(uu[0], 3.5), 1e-9)
}

func TestBatteryRules(t *testing.T) {
	tc := []struct {
		name  string
		in    func(in *BatteryInput)
		modes map[string]BatteryMode
		rule  BatteryRule
	}{
		{
			name:  "above reserve: self-consumption",
			in:    func(in *BatteryInput) {},
			modes: map[string]BatteryMode{"garage": BatteryNormal, "zolder": BatteryNormal},
			rule:  RuleNormal,
		},
		{
			name: "below the reserve: still self-consumption",
			in: func(in *BatteryInput) {
				in.Units = BatteryUnits{garage(20, 0), zolder(20, 0)}
			},
			modes: map[string]BatteryMode{"garage": BatteryNormal, "zolder": BatteryNormal},
			rule:  RuleNormal,
		},
		{
			name: "at the minimum while exporting: self-consumption takes up solar power",
			in: func(in *BatteryInput) {
				in.Units = BatteryUnits{garage(10, 0), zolder(10, 0)}
				in.GridPower = -800
			},
			modes: map[string]BatteryMode{"garage": BatteryNormal, "zolder": BatteryNormal},
			rule:  RuleNormal,
		},
		{
			name: "no measurement: normal",
			in: func(in *BatteryInput) {
				in.Units = BatteryUnits{garage(35, 0), zolder(35, 0)}
				in.Measured = false
			},
			modes: map[string]BatteryMode{"garage": BatteryNormal, "zolder": BatteryNormal},
			rule:  RuleMeter,
		},
		{
			name: "a charging vehicle may use the battery",
			in: func(in *BatteryInput) {
				in.Units = BatteryUnits{garage(20, 300), zolder(20, 400)}
				in.Vehicle = VehicleState{Charging: true}
			},
			modes: map[string]BatteryMode{"garage": BatteryNormal, "zolder": BatteryNormal},
			rule:  RuleNormal,
		},
		{
			name: "planned grid charge, both fit at night",
			in: func(in *BatteryInput) {
				in.Units = BatteryUnits{garage(20, 0), zolder(20, 0)}
				in.GridPower = 250
				in.GridCharge = map[string]bool{"garage": true, "zolder": true}
			},
			modes: map[string]BatteryMode{"garage": BatteryCharge, "zolder": BatteryCharge},
			rule:  RuleGridCharge,
		},
		{
			name: "planned grid charge is skipped while a vehicle charges",
			in: func(in *BatteryInput) {
				in.Units = BatteryUnits{garage(20, 0), zolder(20, 0)}
				in.GridPower = 250
				in.GridCharge = map[string]bool{"garage": true, "zolder": true}
				in.Vehicle = VehicleState{Charging: true}
			},
			modes: map[string]BatteryMode{"garage": BatteryNormal, "zolder": BatteryNormal},
			rule:  RuleNormal,
		},
	}

	for _, tc := range tc {
		t.Run(tc.name, func(t *testing.T) {
			bc := NewBatteryController(DefaultBatterySettings())
			in := batInput(batStart, garage(60, 0), zolder(60, 0))
			tc.in(&in)

			d := bc.Decide(in)
			assert.Equal(t, tc.modes, modes(d))
			for name, r := range rules(d) {
				assert.Equal(t, tc.rule, r, name)
			}
		})
	}
}

// Winter day: the house leaves 1950W, too little for both units. Only the zolder charges.
func TestBatteryGridChargeRoom(t *testing.T) {
	bc := NewBatteryController(DefaultBatterySettings())
	in := batInput(batStart, garage(20, 0), zolder(20, 0))
	in.GridPower = 400
	in.GridCharge = map[string]bool{"garage": true, "zolder": true}

	d := bc.Decide(in)
	assert.Equal(t, map[string]BatteryMode{"garage": BatteryHold, "zolder": BatteryCharge}, modes(d))
	assert.InDelta(t, 1200, d.GridShift, 1e-9, "the zolder starts charging")

	// once charging, the zolder is part of grid power: 400 + 1200, the room stays the same
	in.Now = in.Now.Add(30 * time.Second)
	in.Units = BatteryUnits{garage(20, 0), zolder(20, -1200)}
	in.GridPower = 1600
	d = bc.Decide(in)
	assert.Equal(t, BatteryCharge, modes(d)["zolder"])
	assert.Zero(t, d.GridShift)

	// a full unit does not charge, and holds so it does not discharge into the charging one
	bc = NewBatteryController(DefaultBatterySettings())
	in = batInput(batStart, garage(20, 0), zolder(100, 0))
	in.GridPower = 250
	in.GridCharge = map[string]bool{"garage": true, "zolder": true}
	assert.Equal(t, map[string]BatteryMode{"garage": BatteryCharge, "zolder": BatteryHold}, modes(bc.Decide(in)))
}

// The house draws more than planned while charging: the slot is projected above target, so
// grid charging stops at once, without the calm minute, and is skipped for the rest of the slot
func TestBatteryGridChargeAbort(t *testing.T) {
	bc := NewBatteryController(DefaultBatterySettings())
	in := batInput(batStart, garage(20, 0), zolder(20, 0))
	in.GridPower = 250
	in.GridCharge = map[string]bool{"garage": true, "zolder": true}
	require.Equal(t, BatteryCharge, modes(bc.Decide(in))["zolder"])

	// oven on: 2250 + 2000 charging, projection above target
	in.Now = in.Now.Add(30 * time.Second)
	in.Units = BatteryUnits{garage(20, -800), zolder(20, -1200)}
	in.GridPower = 4250
	in.Budget.Projection = 0.8 // kWh, 3200W average
	d := bc.Decide(in)
	assert.Equal(t, map[string]BatteryMode{"garage": BatteryNormal, "zolder": BatteryNormal}, modes(d))

	// still skipped later in the slot, although the projection is fine again
	in.Now = in.Now.Add(2 * time.Minute)
	in.Units = BatteryUnits{garage(20, 0), zolder(20, 0)}
	in.GridPower = 250
	in.Budget.Projection = 0.5
	assert.Equal(t, BatteryNormal, modes(bc.Decide(in))["zolder"], "rest of the slot skipped")

	// next slot charges again
	in.Now = SlotStart(in.Now).Add(SlotDuration + time.Minute)
	in.Budget.SlotStart = SlotStart(in.Now)
	assert.Equal(t, BatteryCharge, modes(bc.Decide(in))["zolder"])
}

// Availability counts each unit in normal above its minimum: with the garage empty only the
// zolder delivers, 1200W instead of 2000W
func TestBatteryAvailable(t *testing.T) {
	bc := NewBatteryController(DefaultBatterySettings())
	in := batInput(batStart, garage(60, 0), zolder(60, 0))
	in.Reserve = 0
	assert.InDelta(t, 2000, bc.Decide(in).Available, 1e-9)

	in.Units = BatteryUnits{garage(11, 0), zolder(60, 0)}
	assert.InDelta(t, 1200, bc.Decide(in).Available, 1e-9)

	in.Units[1].Reachable = false
	assert.Zero(t, bc.Decide(in).Available)
}

// A discharging unit that is held for grid charging shifts its power to the grid, so the
// loadpoint does not count on it in the same cycle
func TestBatteryGridShift(t *testing.T) {
	bc := NewBatteryController(DefaultBatterySettings())
	in := batInput(batStart, garage(36, 0), zolder(36, 400))
	in.GridPower = 0
	require.Equal(t, BatteryNormal, modes(bc.Decide(in))["garage"])

	// the garage is planned alone: the zolder holds
	in.Now = in.Now.Add(2 * time.Minute)
	in.GridCharge = map[string]bool{"garage": true}
	d := bc.Decide(in)
	assert.Equal(t, map[string]BatteryMode{"garage": BatteryCharge, "zolder": BatteryHold}, modes(d))
	assert.InDelta(t, 800+400, d.GridShift, 1e-9)
}

// An unreachable unit gets no command and keeps its last mode
func TestBatteryUnreachable(t *testing.T) {
	bc := NewBatteryController(DefaultBatterySettings())
	in := batInput(batStart, garage(20, 0), zolder(20, 0))
	in.GridPower = 250
	in.GridCharge = map[string]bool{"zolder": true}
	require.Equal(t, BatteryHold, modes(bc.Decide(in))["garage"])

	in.Now = in.Now.Add(2 * time.Minute)
	in.Units[0].Reachable = false
	in.GridCharge = nil
	d := bc.Decide(in)
	g, _ := d.Mode("garage")
	assert.Equal(t, BatteryHold, g.Mode)
	assert.Equal(t, RuleUnreachable, g.Rule)
	assert.False(t, g.Changed)
	assert.Equal(t, BatteryNormal, modes(d)["zolder"])
}

func TestBatteryWatchdog(t *testing.T) {
	bc := NewBatteryController(DefaultBatterySettings())
	in := batInput(batStart, garage(60, 0), zolder(60, 0))
	in.GridPower = 800
	bc.Decide(in)

	// the garage delivers nothing in normal while the house imports 800W
	in.Units = BatteryUnits{garage(60, 0), zolder(60, 1200)}
	var resends int
	for range 40 {
		in.Now = in.Now.Add(30 * time.Second)
		bc.Decide(in)
		res := bc.Watchdog(in)
		assert.NotContains(t, res.Resend, "zolder")
		resends += len(res.Resend)
		if len(res.Failed) > 0 {
			assert.Equal(t, []string{"garage"}, res.Failed)
			break
		}
	}
	assert.Equal(t, 3, resends, "three resends, then failed")
	assert.Equal(t, []string{"garage"}, bc.Failed())

	// a failed unit stays in normal and no longer charges from the grid
	in.Now = in.Now.Add(2 * time.Minute)
	in.Units = BatteryUnits{garage(20, 0), zolder(20, 0)}
	in.GridPower = 250
	in.GridCharge = map[string]bool{"garage": true, "zolder": true}
	d := bc.Decide(in)
	assert.Equal(t, map[string]BatteryMode{"garage": BatteryNormal, "zolder": BatteryCharge}, modes(d))
	assert.Equal(t, RuleWatchdog, rules(d)["garage"])

	// a unit held for grid charging that keeps discharging
	bc = NewBatteryController(DefaultBatterySettings())
	in = batInput(batStart, garage(20, 0), zolder(35, 0))
	in.GridPower = 250
	in.GridCharge = map[string]bool{"garage": true}
	require.Equal(t, BatteryHold, modes(bc.Decide(in))["zolder"])
	in.Units = BatteryUnits{garage(20, -800), zolder(35, 900)}
	in.Now = in.Now.Add(30 * time.Second)
	assert.Empty(t, bc.Watchdog(in).Resend, "first sighting")
	in.Now = in.Now.Add(time.Minute)
	assert.Empty(t, bc.Watchdog(in).Resend, "within the delay")
	in.Now = in.Now.Add(time.Minute)
	assert.Equal(t, []string{"zolder"}, bc.Watchdog(in).Resend)

	// it follows again: the anomaly timer resets
	in.Units = BatteryUnits{garage(20, -800), zolder(35, 0)}
	in.Now = in.Now.Add(30 * time.Second)
	assert.Empty(t, bc.Watchdog(in).Resend)
}

// A vehicle starting while the units charge from the grid: they stop at once, not after the calm minute
func TestBatteryLeaveGridChargeAtOnce(t *testing.T) {
	bc := NewBatteryController(DefaultBatterySettings())
	in := batInput(batStart, garage(20, 0), zolder(20, 0))
	in.GridPower = 250
	in.GridCharge = map[string]bool{"garage": true, "zolder": true}
	require.Equal(t, BatteryCharge, modes(bc.Decide(in))["zolder"])

	in.Now = in.Now.Add(30 * time.Second)
	in.Units = BatteryUnits{garage(20, -800), zolder(20, -1200)}
	in.GridPower = 2250
	in.Vehicle = VehicleState{Charging: true}
	d := bc.Decide(in)
	assert.Equal(t, map[string]BatteryMode{"garage": BatteryNormal, "zolder": BatteryNormal}, modes(d))
	assert.Equal(t, RuleNormal, rules(d)["zolder"])

	// out of the planned slot too
	bc = NewBatteryController(DefaultBatterySettings())
	in = batInput(batStart, garage(20, 0), zolder(20, 0))
	in.GridPower = 250
	in.GridCharge = map[string]bool{"garage": true, "zolder": true}
	bc.Decide(in)
	in.Now = in.Now.Add(30 * time.Second)
	in.GridCharge = nil
	assert.Equal(t, BatteryNormal, modes(bc.Decide(in))["zolder"])
}

// Into grid charging waits for the calm minute after the last change
func TestBatteryCalm(t *testing.T) {
	bc := NewBatteryController(DefaultBatterySettings())
	in := batInput(batStart, garage(20, 0), zolder(20, 0))
	in.GridPower = 250
	in.GridCharge = map[string]bool{"garage": true, "zolder": true}
	require.Equal(t, BatteryCharge, modes(bc.Decide(in))["zolder"])

	in.Now = in.Now.Add(10 * time.Second)
	in.GridCharge = nil
	require.Equal(t, BatteryNormal, modes(bc.Decide(in))["zolder"], "leaving never waits")

	in.Now = in.Now.Add(10 * time.Second)
	in.GridCharge = map[string]bool{"garage": true, "zolder": true}
	d := bc.Decide(in)
	assert.Equal(t, BatteryNormal, modes(d)["zolder"])
	assert.Equal(t, RuleCalm, rules(d)["zolder"])

	in.Now = in.Now.Add(time.Minute)
	assert.Equal(t, BatteryCharge, modes(bc.Decide(in))["zolder"])
}

// A unit not read while charging from the grid is sent normal, best effort
func TestBatteryUnreachableWhileCharging(t *testing.T) {
	bc := NewBatteryController(DefaultBatterySettings())
	in := batInput(batStart, garage(20, 0), zolder(20, 0))
	in.GridPower = 250
	in.GridCharge = map[string]bool{"garage": true, "zolder": true}
	require.Equal(t, BatteryCharge, modes(bc.Decide(in))["zolder"])

	in.Now = in.Now.Add(30 * time.Second)
	in.Units[1].Reachable = false
	d := bc.Decide(in)
	z, _ := d.Mode("zolder")
	assert.Equal(t, BatteryNormal, z.Mode)
	assert.Equal(t, RuleUnreachable, z.Rule)
	assert.True(t, z.Changed)
}

// NaN, negative or above 100% soc and NaN power count as not read
func TestBatteryInvalidReadings(t *testing.T) {
	for _, u := range []BatteryUnit{zolder(math.NaN(), 0), zolder(-5, 0), zolder(140, 0), zolder(50, math.Inf(1))} {
		assert.False(t, u.valid(), "%v %v", u.Soc, u.Power)
	}

	bc := NewBatteryController(DefaultBatterySettings())
	in := batInput(batStart, garage(20, 0), zolder(math.NaN(), 0))
	in.GridPower = 250
	in.GridCharge = map[string]bool{"garage": true, "zolder": true}
	d := bc.Decide(in)
	assert.Equal(t, RuleUnreachable, rules(d)["zolder"])
	assert.Equal(t, BatteryCharge, modes(d)["garage"])
}

// What a unit can still deliver: its power minus what it delivers already
func TestBatteryAvailableWhileDelivering(t *testing.T) {
	bc := NewBatteryController(DefaultBatterySettings())
	in := batInput(batStart, garage(60, 800), zolder(60, 1200))
	in.Reserve = 0
	assert.Zero(t, bc.Decide(in).Available, "both deliver all they can to the vehicle")

	in.Units = BatteryUnits{garage(60, 300), zolder(60, -500)}
	assert.InDelta(t, 500+1200, bc.Decide(in).Available, 1e-9, "charging from solar does not count against it")
}

// Resends start afresh once the unit follows again: three hiccups days apart do not fail it.
// A unit held for grid charging that charges from solar is no anomaly.
func TestBatteryWatchdogRecovers(t *testing.T) {
	bc := NewBatteryController(DefaultBatterySettings())
	in := batInput(batStart, garage(60, 0), zolder(60, 0))
	in.GridPower = 800
	bc.Decide(in)

	for day := range 5 {
		in.Now = batStart.Add(time.Duration(day) * 24 * time.Hour)
		in.Units = BatteryUnits{garage(60, 0), zolder(60, 1200)}
		for range 6 {
			in.Now = in.Now.Add(30 * time.Second)
			assert.Empty(t, bc.Watchdog(in).Failed, "day %d", day)
		}
		in.Units = BatteryUnits{garage(60, 400), zolder(60, 400)}
		in.Now = in.Now.Add(30 * time.Second)
		bc.Watchdog(in)
	}
	assert.Empty(t, bc.Failed())

	bc = NewBatteryController(DefaultBatterySettings())
	in = batInput(batStart, garage(20, 0), zolder(35, 0))
	in.GridPower = 250
	in.GridCharge = map[string]bool{"garage": true}
	bc.Decide(in)
	in.Units = BatteryUnits{garage(20, -800), zolder(35, -800)}
	for range 20 {
		in.Now = in.Now.Add(30 * time.Second)
		assert.Empty(t, bc.Watchdog(in).Resend)
	}
}

// Invariants over random cycles with a plant that follows the decided modes
func TestBatteryInvariants(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for range 300 {
		bc := NewBatteryController(DefaultBatterySettings())
		now := time.Date(2026, 11, 3, 5, 0, 0, 0, Location)
		gs, zs := 15+rng.Float64()*50, 15+rng.Float64()*50
		var gp, zp float64

		for range 200 {
			now = now.Add(time.Duration(5+rng.Intn(30)) * time.Second)
			in := batInput(now, garage(gs, gp), zolder(zs, zp))
			in.Measured = rng.Float64() > 0.05
			in.Vehicle.Charging = rng.Float64() < 0.3
			in.GridPower = -500 + rng.Float64()*3500
			in.Budget.Rest = 1000 + rng.Float64()*1350
			if rng.Float64() < 0.1 {
				in.Budget.Projection = 0.8
			}
			if rng.Float64() < 0.5 {
				in.GridCharge = map[string]bool{"garage": rng.Float64() < 0.7, "zolder": rng.Float64() < 0.7}
			}
			if rng.Float64() < 0.05 {
				in.Units[rng.Intn(2)].Reachable = false
			}

			d := bc.Decide(in)
			for i, u := range d.Units {
				unit := in.Units[i]
				if !unit.valid() {
					assert.NotEqual(t, BatteryCharge, u.Mode, "charge for a unit not read")
					continue
				}
				assert.False(t, u.Mode == BatteryCharge && in.Vehicle.Charging, "charge while a vehicle charges")
				assert.False(t, u.Mode == BatteryHold && !slices.Contains(modeList(d), BatteryCharge) && u.Rule != RuleCalm, "hold without grid charging")
				assert.False(t, u.Mode != BatteryNormal && !in.Measured, "not normal without measurement")
				assert.False(t, u.Mode == BatteryCharge && !in.GridCharge[u.Name], "charge outside a planned slot")
				assert.False(t, u.Mode == BatteryCharge && unit.full(), "charge when full")
			}

			gp, zp = 0, 0
			if m := modes(d)["garage"]; m == BatteryCharge {
				gp, gs = -800, math.Min(100, gs+0.1)
			} else if m == BatteryNormal && gs > 10 {
				gp, gs = 300, gs-0.05
			}
			if m := modes(d)["zolder"]; m == BatteryCharge {
				zp, zs = -1200, math.Min(100, zs+0.1)
			} else if m == BatteryNormal && zs > 10 {
				zp, zs = 450, zs-0.05
			}
		}
	}
}

// The installation as configured in evcc: both units charge at 2000W and discharge at 1000W.
// At night the house leaves 2100W, room for one unit at a time, the larger charge first; both
// together deliver 2000W for a peak.
func TestBatteryChargeDischargePowers(t *testing.T) {
	g, z := garage(20, 0), zolder(20, 0)
	g.ChargePower, g.DischargePower = 2000, 1000
	z.ChargePower, z.DischargePower = 2000, 1000

	bc := NewBatteryController(DefaultBatterySettings())
	in := batInput(batStart, g, z)
	in.GridPower = 250
	in.GridCharge = map[string]bool{"garage": true, "zolder": true}
	d := bc.Decide(in)
	assert.Equal(t, map[string]BatteryMode{"garage": BatteryCharge, "zolder": BatteryHold}, modes(d), "one unit at a time")
	assert.InDelta(t, 2000, d.GridShift, 1e-9)

	// the house at 400W leaves 1950W: no grid charging at all
	bc = NewBatteryController(DefaultBatterySettings())
	in.GridPower = 400
	d = bc.Decide(in)
	assert.NotContains(t, []BatteryMode{modes(d)["garage"], modes(d)["zolder"]}, BatteryCharge)

	in.GridCharge, in.Reserve = nil, 0
	in.Units = BatteryUnits{g, z}
	in.Units[0].Soc, in.Units[1].Soc = 60, 60
	assert.InDelta(t, 2000, NewBatteryController(DefaultBatterySettings()).Decide(in).Available, 1e-9)
}

// The units on 2 November: evcc sent normal, the Indevolts stayed in stand-by and 1.4 kW went
// to the grid. A unit in normal that takes nothing while the site exports gets its mode again.
func TestBatteryWatchdogExport(t *testing.T) {
	bc := NewBatteryController(DefaultBatterySettings())
	in := batInput(batStart, garage(10, 0), zolder(10, 0))
	in.GridPower = -1200
	bc.Decide(in)

	in.Now = in.Now.Add(30 * time.Second)
	assert.Empty(t, bc.Watchdog(in).Resend, "first sighting")
	in.Now = in.Now.Add(2 * time.Minute)
	assert.ElementsMatch(t, []string{"garage", "zolder"}, bc.Watchdog(in).Resend)

	// the garage charges at its maximum, the rest still goes to the grid: only the zolder misbehaves
	bc = NewBatteryController(DefaultBatterySettings())
	in.Units = BatteryUnits{garage(10, -800), zolder(10, 0)}
	in.GridPower = -400
	bc.Decide(in)
	var resend []string
	for range 6 {
		in.Now = in.Now.Add(30 * time.Second)
		resend = append(resend, bc.Watchdog(in).Resend...)
	}
	assert.Equal(t, []string{"zolder"}, resend)

	// a full unit, or a small export, is no anomaly
	bc = NewBatteryController(DefaultBatterySettings())
	in.Units = BatteryUnits{garage(98, 0), zolder(10, 0)}
	in.GridPower = -200
	bc.Decide(in)
	for range 10 {
		in.Now = in.Now.Add(30 * time.Second)
		assert.Empty(t, bc.Watchdog(in).Resend)
	}
	in.GridPower = -1000
	for range 10 {
		in.Now = in.Now.Add(30 * time.Second)
		assert.NotContains(t, bc.Watchdog(in).Resend, "garage", "full")
	}

	// a unit that never takes export, e.g. in frost: three resends, never failed
	bc = NewBatteryController(DefaultBatterySettings())
	in.Units = BatteryUnits{garage(10, 0), zolder(10, -1200)}
	in.GridPower = -800
	bc.Decide(in)
	var resends int
	for range 40 {
		in.Now = in.Now.Add(30 * time.Second)
		res := bc.Watchdog(in)
		resends += len(res.Resend)
		assert.Empty(t, res.Failed)
	}
	assert.Equal(t, 3, resends)
	assert.Empty(t, bc.Failed())
}

// While the modes are not sent, e.g. during an external mode, the watchdog starts afresh
func TestBatteryResetWatchdog(t *testing.T) {
	bc := NewBatteryController(DefaultBatterySettings())
	in := batInput(batStart, garage(60, 0), zolder(60, 1200))
	in.GridPower = 800
	bc.Decide(in)

	for range 5 {
		in.Now = in.Now.Add(30 * time.Second)
		bc.Watchdog(in)
	}
	bc.ResetWatchdog()

	in.Now = in.Now.Add(30 * time.Second)
	assert.Empty(t, bc.Watchdog(in).Resend, "first sighting again")
	in.Now = in.Now.Add(time.Minute)
	assert.Empty(t, bc.Watchdog(in).Resend, "within the delay")
}

func modeList(d BatteryDecision) []BatteryMode {
	var res []BatteryMode
	for _, u := range d.Units {
		res = append(res, u.Mode)
	}
	return res
}
