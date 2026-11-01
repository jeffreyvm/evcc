package core

import (
	"slices"
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/capacity"
	"github.com/evcc-io/evcc/util/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// capBattery is an Indevolt-like battery: soc, capacity, soc limits and modes. Its power is set by
// the test and guarded by the installation lock, like the rest of the rig.
type capBattery struct {
	inst *capInstallation

	soc, capacity float64
	power         float64 // W, positive discharging
	modes         []api.BatteryMode
	err           error // returned by SetBatteryMode, e.g. a unit hanging after a restart
}

func (b *capBattery) CurrentPower() (float64, error) {
	b.inst.mu.Lock()
	defer b.inst.mu.Unlock()
	return b.power, nil
}

func (b *capBattery) Soc() (float64, error) {
	b.inst.mu.Lock()
	defer b.inst.mu.Unlock()
	return b.soc, nil
}

func (b *capBattery) Capacity() (float64, error) {
	return b.capacity, nil
}

func (b *capBattery) GetSocLimits() (float64, float64, error) {
	return 10, 100, nil
}

func (b *capBattery) BatteryModes() []api.BatteryMode {
	return []api.BatteryMode{api.BatteryNormal, api.BatteryHold, api.BatteryCharge}
}

func (b *capBattery) SetBatteryMode(mode api.BatteryMode) error {
	b.inst.mu.Lock()
	defer b.inst.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	b.modes = append(b.modes, mode)
	return nil
}

func (b *capBattery) set(soc, power float64) {
	b.inst.mu.Lock()
	defer b.inst.mu.Unlock()
	b.soc, b.power = soc, power
}

func (b *capBattery) sent() []api.BatteryMode {
	b.inst.mu.Lock()
	defer b.inst.mu.Unlock()
	return slices.Clone(b.modes)
}

// newBatteryRig is the contract rig with the garage (4 kWh) and the zolder (6 kWh), minimum 10%.
// The rig starts on 20 October: without house history the reserve is the season floor of 15%
// plus the minimum, 2.5 kWh, so 25% on both.
func newBatteryRig(t *testing.T, batteryMode string, soc float64, setup func(r *capRig)) (*capRig, *capBattery, *capBattery) {
	t.Helper()

	var garage, zolder *capBattery
	r := newCapRig(t, CapacityOn, func(r *capRig) {
		garage = &capBattery{inst: r.inst, soc: soc, capacity: 4}
		zolder = &capBattery{inst: r.inst, soc: soc, capacity: 6}
		r.inst.batteries = []*capBattery{garage, zolder}
		r.site.batteryMeters = []config.Device[api.Meter]{
			config.NewStaticDevice(config.Named{Name: "garage"}, api.Meter(garage)),
			config.NewStaticDevice(config.Named{Name: "zolder"}, api.Meter(zolder)),
		}
		if setup != nil {
			setup(r)
		}
	})

	require.NoError(t, r.site.SetCapacityBatteryMode(batteryMode))
	require.NoError(t, r.site.SetCapacityBatteryPower("garage", 800))
	require.NoError(t, r.site.SetCapacityBatteryPower("zolder", 1200))

	return r, garage, zolder
}

func batteryUnitStatus(t *testing.T, r *capRig, name string) capacityBatteryUnitStatus {
	t.Helper()
	st := r.site.capacityBatteryStatus()
	i := slices.IndexFunc(st.Units, func(u capacityBatteryUnitStatus) bool { return u.Name == name })
	require.GreaterOrEqual(t, i, 0, "unit %s", name)
	return st.Units[i]
}

// Shadow mode decides and publishes, but sends nothing to the batteries
func TestCapacityBatteryContractShadow(t *testing.T) {
	r, garage, zolder := newBatteryRig(t, CapacityShadow, 20, func(r *capRig) {
		r.inst.connected = false
		r.lp.status = api.StatusA
	})

	r.cycles(4, 30*time.Second)

	assert.Empty(t, garage.sent())
	assert.Empty(t, zolder.sent())

	st := r.site.capacityBatteryStatus()
	assert.Equal(t, CapacityShadow, st.Mode)
	assert.InDelta(t, 2.5, st.Reserve, 1e-9, "season floor 15% plus 10% minimum")
	require.NotNil(t, st.Plan)
	assert.True(t, st.Plan.Fixed, "no house history")

	z := batteryUnitStatus(t, r, "zolder")
	assert.Equal(t, capacity.BatteryHold, z.Mode)
	assert.Equal(t, capacity.RuleReserve, z.Rule)
	assert.InDelta(t, 1.5, z.Reserve, 1e-9)
}

// On: at the reserve each battery is held once, and a peak threat releases them
func TestCapacityBatteryContractHoldAndPeak(t *testing.T) {
	r, garage, zolder := newBatteryRig(t, CapacityOn, 20, func(r *capRig) {
		r.inst.connected = false
		r.lp.status = api.StatusA
	})

	r.cycles(4, 30*time.Second)
	assert.Equal(t, []api.BatteryMode{api.BatteryHold}, garage.sent())
	assert.Equal(t, []api.BatteryMode{api.BatteryHold}, zolder.sent())
	assert.Equal(t, api.BatteryHold, r.site.GetBatteryMode())

	// oven and cooktop: 3000W above the 2350W target, after 30s the peak threatens
	r.inst.set(func(inst *capInstallation) { inst.house = 3000 })
	r.cycles(3, 30*time.Second)

	assert.Equal(t, []api.BatteryMode{api.BatteryHold, api.BatteryNormal}, zolder.sent())
	assert.Equal(t, capacity.RulePeak, batteryUnitStatus(t, r, "zolder").Rule)
	assert.Equal(t, api.BatteryNormal, r.site.GetBatteryMode())
}

// Without grid measurement the batteries go to normal at once
func TestCapacityBatteryContractMeterLoss(t *testing.T) {
	r, _, zolder := newBatteryRig(t, CapacityOn, 20, func(r *capRig) {
		r.inst.connected = false
		r.lp.status = api.StatusA
	})

	r.cycles(2, 30*time.Second)
	require.Equal(t, []api.BatteryMode{api.BatteryHold}, zolder.sent())

	r.inst.set(func(inst *capInstallation) { inst.gridErr = assert.AnError })
	r.cycles(2, 30*time.Second)

	assert.Equal(t, []api.BatteryMode{api.BatteryHold, api.BatteryNormal}, zolder.sent())
	assert.Equal(t, capacity.RuleMeter, batteryUnitStatus(t, r, "zolder").Rule)
}

// An external mode goes first: the regular battery control applies it
func TestCapacityBatteryContractExternal(t *testing.T) {
	r, _, zolder := newBatteryRig(t, CapacityOn, 50, func(r *capRig) {
		r.inst.connected = false
		r.lp.status = api.StatusA
	})

	r.cycle(30 * time.Second)
	require.Equal(t, []api.BatteryMode{api.BatteryNormal}, zolder.sent(), "normal once, the mode was unknown")

	require.NoError(t, r.site.SetBatteryModeExternal(api.BatteryCharge))
	r.cycle(30 * time.Second)
	assert.Equal(t, []api.BatteryMode{api.BatteryNormal, api.BatteryCharge}, zolder.sent())
}

// The battery decides first: a held battery that was feeding the vehicle no longer counts in the
// same cycle. Fast mode, the batteries help down to the buffer of 1.5 kWh, 15% on both.
//
// House 500W, vehicle at 8A (1840W), batteries 600 + 900W: grid 840W. When they are held, grid
// will be 2340W, so the ceiling is 1840 + 2350 − 2340 = 1850W = 8.0A, not 3350W as with the
// battery power that is about to disappear.
func TestCapacityBatteryContractGridShift(t *testing.T) {
	r, garage, zolder := newBatteryRig(t, CapacityOn, 16, nil)

	r.cycle(30 * time.Second)
	garage.set(16, 600)
	zolder.set(16, 900)
	r.cycles(3, 30*time.Second)
	require.Equal(t, capacity.BatteryNormal, batteryUnitStatus(t, r, "zolder").Mode, "above the buffer: helps the vehicle")

	// at the buffer, a minute later
	garage.set(15, 600)
	zolder.set(15, 900)
	r.cycle(time.Minute)

	assert.Equal(t, capacity.BatteryHold, batteryUnitStatus(t, r, "zolder").Mode)
	assert.Contains(t, zolder.sent(), api.BatteryHold)

	// dead time and the slot energy so far move the ceiling by a few 0.1A, never to 14.5A
	current, _ := r.inst.state()
	assert.InDelta(t, capCeilingCurrent, current, 0.5, "ceiling without the held battery power")
}

// With battery control on, an appliance is covered only by the units in normal above their minimum
func TestCapacityBatteryContractAvailable(t *testing.T) {
	r, garage, _ := newBatteryRig(t, CapacityOn, 60, func(r *capRig) {
		r.inst.connected = false
		r.lp.status = api.StatusA
	})

	r.cycle(30 * time.Second)
	assert.InDelta(t, 2000, r.site.capacityBatteryAvailable(), 1e-9)

	garage.set(11, 0)
	r.cycle(30 * time.Second)
	assert.InDelta(t, 1200, r.site.capacityBatteryAvailable(), 1e-9, "garage at its minimum")
}

// Switching battery control off hands the batteries back to the regular control, which puts a
// held battery back in normal
func TestCapacityBatteryContractOff(t *testing.T) {
	r, _, zolder := newBatteryRig(t, CapacityOn, 20, func(r *capRig) {
		r.inst.connected = false
		r.lp.status = api.StatusA
	})

	r.cycles(2, 30*time.Second)
	require.Equal(t, []api.BatteryMode{api.BatteryHold}, zolder.sent())

	require.NoError(t, r.site.SetCapacityBatteryMode(CapacityOff))
	r.cycle(30 * time.Second)
	assert.Equal(t, []api.BatteryMode{api.BatteryHold, api.BatteryNormal}, zolder.sent())
	assert.Equal(t, CapacityOff, r.site.capacityBatteryStatus().Mode)
}

func TestCapacityBatterySettings(t *testing.T) {
	r, _, _ := newBatteryRig(t, CapacityOff, 50, nil)
	s := r.site

	assert.Error(t, s.SetCapacityBatteryMode("auto"))
	assert.Error(t, s.SetCapacityFastBattery("all"))
	assert.Error(t, s.SetCapacityReserveMode("auto"))
	assert.Error(t, s.SetCapacityBatteryPower("kelder", 800), "unknown battery")
	assert.Error(t, s.SetCapacityBatteryPower("garage", -1))
	assert.Error(t, s.SetCapacityBatteryPower("garage", 20000))

	require.NoError(t, s.SetCapacityFastBattery("reserve"))
	assert.Equal(t, "reserve", s.GetCapacityFastBattery())
	require.NoError(t, s.SetCapacityReserveMode("fixed"))
	assert.Equal(t, "fixed", s.GetCapacityReserveMode())
	require.NoError(t, s.SetCapacityGridCharge(true))
	assert.True(t, s.GetCapacityGridCharge())

	assert.Equal(t, map[string]float64{"garage": 800, "zolder": 1200}, s.GetCapacityBatteryPowers())
	require.NoError(t, s.SetCapacityBatteryPower("garage", 0))
	assert.Equal(t, map[string]float64{"zolder": 1200}, s.GetCapacityBatteryPowers(), "0 estimates again")

	// without a controllable battery battery control cannot be switched on
	s.batteryMeters = nil
	assert.Error(t, s.SetCapacityBatteryMode(CapacityOn))
}

func TestEffectiveBatteryMode(t *testing.T) {
	for _, tc := range []struct{ capacity, battery, res string }{
		{CapacityOff, CapacityOn, CapacityOff},
		{CapacityOn, CapacityOff, CapacityOff},
		{CapacityShadow, CapacityOn, CapacityShadow},
		{CapacityOn, CapacityShadow, CapacityShadow},
		{CapacityOn, CapacityOn, CapacityOn},
	} {
		assert.Equal(t, tc.res, effectiveBatteryMode(tc.capacity, tc.battery), "%s/%s", tc.capacity, tc.battery)
	}
}

// The watchdog only checks modes that were sent: during an external mode the battery follows
// the external mode, which is no reason to fail it
func TestCapacityBatteryContractExternalNoWatchdog(t *testing.T) {
	r, _, zolder := newBatteryRig(t, CapacityOn, 20, func(r *capRig) {
		r.inst.connected = false
		r.lp.status = api.StatusA
	})

	r.cycles(2, 30*time.Second)
	require.NoError(t, r.site.SetBatteryModeExternal(api.BatteryCharge))
	zolder.set(20, -1200)

	// 12 minutes, the external mode kept alive like Home Assistant does
	for range 24 {
		require.NoError(t, r.site.SetBatteryModeExternal(api.BatteryCharge))
		r.cycle(30 * time.Second)
	}
	assert.False(t, batteryUnitStatus(t, r, "zolder").Failed)
	assert.Empty(t, r.site.capacityCtl.bat.resend, "no resends queued while not controlling")
}

// gridChargeRig is at 16:45 on 20 October with grid charging on: without house history and
// forecast the reserve is the season floor, 2.5 kWh, and the units at 20% need 0.5 kWh, planned
// in the last slots before 17:00
func gridChargeRig(t *testing.T, setup func(r *capRig)) (*capRig, *capBattery, *capBattery) {
	t.Helper()
	r, garage, zolder := newBatteryRig(t, CapacityOn, 20, setup)
	require.NoError(t, r.site.SetCapacityGridCharge(true))
	r.clk.Add(6*time.Hour + 45*time.Minute)
	return r, garage, zolder
}

// In a planned slot the zolder charges: the house leaves 1850W, too little for both, so the
// garage holds and does not discharge into it
func TestCapacityBatteryContractGridCharge(t *testing.T) {
	r, garage, zolder := gridChargeRig(t, func(r *capRig) {
		r.inst.connected = false
		r.lp.status = api.StatusA
	})

	r.cycle(30 * time.Second)

	st := r.site.capacityBatteryStatus()
	require.NotNil(t, st.Plan)
	require.NotEmpty(t, st.Plan.Slots)
	assert.InDelta(t, 0.5, st.Plan.Need, 1e-9)

	assert.Equal(t, []api.BatteryMode{api.BatteryCharge}, zolder.sent())
	assert.Equal(t, []api.BatteryMode{api.BatteryHold}, garage.sent())
	assert.Equal(t, api.BatteryCharge, r.site.GetBatteryMode())
}

// A vehicle that wants to charge fast keeps the batteries from grid charging, also while the
// capacity limit has not let it start yet
func TestCapacityBatteryContractGridChargeVehicle(t *testing.T) {
	r, _, zolder := gridChargeRig(t, func(r *capRig) {
		r.inst.enabled = false
		r.lp.status = api.StatusB
	})

	r.cycles(4, 30*time.Second)
	assert.NotContains(t, zolder.sent(), api.BatteryCharge)
}

// The profile is looked up by wall clock: on 25 October (100 slots) 17:00 after the switch
// still gets the 17:00 value
func TestCapacityProfileSeriesDST(t *testing.T) {
	var profile [96]float64
	for i := range profile {
		profile[i] = float64(i) / 1000 // kWh, slot index in Wh
	}

	at := time.Date(2026, 10, 25, 0, 0, 0, 0, capacity.Location)
	res := profileSeries(&profile, at, 101, capacity.Location)

	// 02:00 to 03:00 is there twice, so 17:00 is slot 72 of the day
	assert.InDelta(t, capacity.SlotPower(68.0/1000), res[72], 1e-9, "17:00")
	assert.InDelta(t, capacity.SlotPower(8.0/1000), res[8], 1e-9, "02:00 first")
	assert.InDelta(t, capacity.SlotPower(8.0/1000), res[12], 1e-9, "02:00 again")
	assert.InDelta(t, capacity.SlotPower(92.0/1000), res[96], 1e-9, "23:00, 24 hours after midnight")
	assert.InDelta(t, capacity.SlotPower(0), res[100], 1e-9, "next midnight")
}

// One unit hanging after a restart does not keep the other from being controlled: battery
// control sends each unit its own mode and retries the hanging one
func TestCapacityBatteryContractStartWithHangingUnit(t *testing.T) {
	r, garage, zolder := newBatteryRig(t, CapacityOn, 20, func(r *capRig) {
		r.inst.connected = false
		r.lp.status = api.StatusA
	})
	garage.err = assert.AnError

	r.site.capacityBatteryStart()
	r.cycle(30 * time.Second)
	assert.Equal(t, []api.BatteryMode{api.BatteryHold}, zolder.sent(), "zolder held at the reserve right away")

	r.cycle(30 * time.Second)
	assert.Equal(t, "on", r.site.capacityBatteryStatus().Control)

	r.inst.set(func(*capInstallation) { garage.err = nil })
	r.cycle(30 * time.Second)
	assert.Equal(t, []api.BatteryMode{api.BatteryHold}, garage.sent(), "garage gets its mode once it answers")
}
