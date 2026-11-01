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
