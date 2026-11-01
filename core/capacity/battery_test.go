package capacity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The installation of the design: garage 4 kWh / 800 W, zolder 6 kWh / 1200 W, minimum 10%.
// A reserve target of 3.5 kWh splits by capacity: garage 1.4 kWh (35%), zolder 2.1 kWh (35%).

var batStart = time.Date(2026, 11, 3, 20, 0, 0, 0, Location) // Tuesday evening, after the cooking window

func garage(soc, power float64) BatteryUnit {
	return BatteryUnit{Name: "garage", Reachable: true, Soc: soc, MinSoc: 10, MaxSoc: 100, Capacity: 4, Power: power, MaxPower: 800}
}

func zolder(soc, power float64) BatteryUnit {
	return BatteryUnit{Name: "zolder", Reachable: true, Soc: soc, MinSoc: 10, MaxSoc: 100, Capacity: 6, Power: power, MaxPower: 1200}
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

	// an unreachable unit drops out: the other carries the whole reserve
	uu[0].Reachable = false
	assert.InDelta(t, 3.5, uu.share(uu[1], 3.5), 1e-9)
	assert.Zero(t, uu.share(uu[0], 3.5))
}

func TestBatteryRules(t *testing.T) {
	tc := []struct {
		name   string
		in     func(in *BatteryInput)
		modes  map[string]BatteryMode
		rule   BatteryRule
		energy float64 // kWh garage, zolder are set below
	}{
		{
			name:  "above reserve: self-consumption",
			in:    func(in *BatteryInput) {},
			modes: map[string]BatteryMode{"garage": BatteryNormal, "zolder": BatteryNormal},
			rule:  RuleNormal,
		},
		{
			name: "at the reserve: hold",
			in: func(in *BatteryInput) {
				in.Units = BatteryUnits{garage(35, 0), zolder(35, 0)}
			},
			modes: map[string]BatteryMode{"garage": BatteryHold, "zolder": BatteryHold},
			rule:  RuleReserve,
		},
		{
			name: "peak threat releases the reserve",
			in: func(in *BatteryInput) {
				in.Units = BatteryUnits{garage(35, 0), zolder(35, 0)}
				in.Threat = true
			},
			modes: map[string]BatteryMode{"garage": BatteryNormal, "zolder": BatteryNormal},
			rule:  RulePeak,
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
			name: "smart charging uses the battery down to the reserve",
			in: func(in *BatteryInput) {
				in.Units = BatteryUnits{garage(36, 300), zolder(36, 400)}
				in.Vehicle = VehicleState{Charging: true}
			},
			modes: map[string]BatteryMode{"garage": BatteryNormal, "zolder": BatteryNormal},
			rule:  RuleVehicle,
		},
		{
			name: "smart charging holds at the reserve",
			in: func(in *BatteryInput) {
				in.Units = BatteryUnits{garage(35, 300), zolder(35, 400)}
				in.Vehicle = VehicleState{Charging: true}
			},
			modes: map[string]BatteryMode{"garage": BatteryHold, "zolder": BatteryHold},
			rule:  RuleVehicle,
		},
		{
			// buffer 1.5 kWh: garage 0.6 kWh (15%), zolder 0.9 kWh (15%)
			name: "fast charging uses the battery below the reserve, down to the buffer",
			in: func(in *BatteryInput) {
				in.Units = BatteryUnits{garage(20, 300), zolder(20, 400)}
				in.Vehicle = VehicleState{Charging: true, Fast: true}
			},
			modes: map[string]BatteryMode{"garage": BatteryNormal, "zolder": BatteryNormal},
			rule:  RuleVehicle,
		},
		{
			name: "fast charging holds at the buffer",
			in: func(in *BatteryInput) {
				in.Units = BatteryUnits{garage(15, 300), zolder(15, 400)}
				in.Vehicle = VehicleState{Charging: true, Fast: true}
			},
			modes: map[string]BatteryMode{"garage": BatteryHold, "zolder": BatteryHold},
			rule:  RuleVehicle,
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
			modes: map[string]BatteryMode{"garage": BatteryHold, "zolder": BatteryHold},
			rule:  RuleVehicle,
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

	// a full unit does not charge
	bc = NewBatteryController(DefaultBatterySettings())
	in = batInput(batStart, garage(20, 0), zolder(100, 0))
	in.GridPower = 250
	in.GridCharge = map[string]bool{"garage": true, "zolder": true}
	assert.Equal(t, map[string]BatteryMode{"garage": BatteryCharge, "zolder": BatteryNormal}, modes(bc.Decide(in)))
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
	assert.Equal(t, map[string]BatteryMode{"garage": BatteryHold, "zolder": BatteryHold}, modes(d))

	// still skipped later in the slot, although the projection is fine again
	in.Now = in.Now.Add(2 * time.Minute)
	in.Units = BatteryUnits{garage(20, 0), zolder(20, 0)}
	in.GridPower = 250
	in.Budget.Projection = 0.5
	assert.Equal(t, BatteryHold, modes(bc.Decide(in))["zolder"], "rest of the slot skipped")

	// next slot charges again
	in.Now = SlotStart(in.Now).Add(SlotDuration + time.Minute)
	in.Budget.SlotStart = SlotStart(in.Now)
	assert.Equal(t, BatteryCharge, modes(bc.Decide(in))["zolder"])
}

// Holding at the reserve releases only 2% above it, and the mode changes at most once a minute
func TestBatteryReserveHysteresisAndCalm(t *testing.T) {
	bc := NewBatteryController(DefaultBatterySettings())
	now := batStart

	step := func(soc float64) BatteryDecision {
		in := batInput(now, garage(soc, 0), zolder(soc, 0))
		return bc.Decide(in)
	}

	assert.Equal(t, BatteryHold, modes(step(35))["zolder"])

	// 36% is above the reserve but within the hysteresis
	now = now.Add(2 * time.Minute)
	assert.Equal(t, BatteryHold, modes(step(36))["zolder"])

	now = now.Add(2 * time.Minute)
	d := step(37.5)
	assert.Equal(t, BatteryNormal, modes(d)["zolder"], "released above 37%")

	// back at the reserve 10s later: calm keeps normal for a minute
	now = now.Add(10 * time.Second)
	d = step(35)
	assert.Equal(t, BatteryNormal, modes(d)["zolder"])
	assert.Equal(t, RuleCalm, rules(d)["zolder"])

	now = now.Add(time.Minute)
	assert.Equal(t, BatteryHold, modes(step(35))["zolder"])

	// a peak threat is never delayed by calm
	now = now.Add(5 * time.Second)
	in := batInput(now, garage(35, 0), zolder(35, 0))
	in.Threat = true
	assert.Equal(t, BatteryNormal, modes(bc.Decide(in))["zolder"])
}

// Exporting for a minute releases a held unit, so it takes up solar power, and keeps it
// released while it charges
func TestBatteryInjection(t *testing.T) {
	bc := NewBatteryController(DefaultBatterySettings())
	now := batStart

	in := batInput(now, garage(35, 0), zolder(35, 0))
	require.Equal(t, BatteryHold, modes(bc.Decide(in))["zolder"])

	in.GridPower = -400
	for range 2 {
		in.Now = in.Now.Add(30 * time.Second)
		bc.Decide(in)
	}
	in.Now = in.Now.Add(30 * time.Second)
	d := bc.Decide(in)
	assert.Equal(t, BatteryNormal, modes(d)["zolder"])
	assert.Equal(t, RuleInjection, rules(d)["zolder"])

	// now charging from solar, grid near zero: stays released
	in.Now = in.Now.Add(30 * time.Second)
	in.GridPower = 0
	in.Units = BatteryUnits{garage(35, -150), zolder(35, -250)}
	assert.Equal(t, RuleInjection, rules(bc.Decide(in))["zolder"])

	// no longer charging: the reserve rule applies again
	in.Now = in.Now.Add(2 * time.Minute)
	in.Units = BatteryUnits{garage(35, 0), zolder(35, 0)}
	assert.Equal(t, BatteryHold, modes(bc.Decide(in))["zolder"])
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

// A discharging unit that is held shifts its power to the grid, so the loadpoint does not
// count on it in the same cycle
func TestBatteryGridShift(t *testing.T) {
	bc := NewBatteryController(DefaultBatterySettings())
	in := batInput(batStart, garage(36, 600), zolder(36, 900))
	in.Vehicle = VehicleState{Charging: true}
	require.Equal(t, BatteryNormal, modes(bc.Decide(in))["zolder"])

	in.Now = in.Now.Add(2 * time.Minute)
	in.Units = BatteryUnits{garage(35, 600), zolder(35, 900)}
	d := bc.Decide(in)
	assert.Equal(t, BatteryHold, modes(d)["zolder"])
	assert.InDelta(t, 1500, d.GridShift, 1e-9)
}

// An unreachable unit gets no command and keeps its last mode
func TestBatteryUnreachable(t *testing.T) {
	bc := NewBatteryController(DefaultBatterySettings())
	in := batInput(batStart, garage(35, 0), zolder(35, 0))
	require.Equal(t, BatteryHold, modes(bc.Decide(in))["garage"])

	in.Now = in.Now.Add(2 * time.Minute)
	in.Units[0].Reachable = false
	in.Threat = true
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

	// a failed unit stays in normal, even at the reserve
	in.Now = in.Now.Add(2 * time.Minute)
	in.Units = BatteryUnits{garage(35, 0), zolder(35, 0)}
	d := bc.Decide(in)
	assert.Equal(t, map[string]BatteryMode{"garage": BatteryNormal, "zolder": BatteryHold}, modes(d))
	assert.Equal(t, RuleWatchdog, rules(d)["garage"])

	// a held unit that keeps discharging
	bc = NewBatteryController(DefaultBatterySettings())
	in = batInput(batStart, garage(35, 0), zolder(35, 0))
	bc.Decide(in)
	in.Units = BatteryUnits{garage(35, 0), zolder(35, 900)}
	in.Now = in.Now.Add(30 * time.Second)
	assert.Empty(t, bc.Watchdog(in).Resend, "first sighting")
	in.Now = in.Now.Add(time.Minute)
	assert.Empty(t, bc.Watchdog(in).Resend, "within the delay")
	in.Now = in.Now.Add(time.Minute)
	assert.Equal(t, []string{"zolder"}, bc.Watchdog(in).Resend)

	// it follows again: the anomaly timer resets
	in.Units = BatteryUnits{garage(35, 0), zolder(35, 0)}
	in.Now = in.Now.Add(30 * time.Second)
	assert.Empty(t, bc.Watchdog(in).Resend)
}
