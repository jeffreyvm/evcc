package core

import (
	"slices"
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
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

// newBatteryRig is the contract rig with the garage (4 kWh) and the zolder (6 kWh), minimum 10%
func newBatteryRig(t *testing.T, soc float64, setup func(r *capRig)) (*capRig, *capBattery, *capBattery) {
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

	return r, garage, zolder
}

// evcc never sends a battery mode, also not on the paths upstream holds the batteries on: an
// external hold via REST or MQTT, and discharge control while the vehicle fast charges.
// On an Indevolt hold is real-time control, in which the unit no longer stores the solar surplus.
func TestEmsBatteryModeNeverSent(t *testing.T) {
	r, garage, zolder := newBatteryRig(t, 60, nil)
	require.NoError(t, r.site.SetBatteryDischargeControl(true))
	require.NoError(t, r.site.SetBatteryModeExternal(api.BatteryHold))

	// sunny: the batteries store the surplus on their own
	r.inst.set(func(inst *capInstallation) { inst.pv = 4000 })
	garage.set(60, -800)
	zolder.set(60, -1200)

	r.cycles(10, 30*time.Second)

	assert.Empty(t, garage.sent(), "garage gets no mode")
	assert.Empty(t, zolder.sent(), "zolder gets no mode")
	assert.Equal(t, api.BatteryUnknown, r.site.GetBatteryMode(), "no mode recorded either")
}

// Battery power is no headroom for the vehicle: with both batteries delivering 2000W the fast
// charging ceiling stays at 8.0A, as without them. Otherwise the limit raised the vehicle by the
// battery power, and a battery covering a house peak, e.g. with the peak shaving of the Indevolt,
// emptied its reserve into the vehicle.
func TestCapacityContractBatteryNoHeadroom(t *testing.T) {
	r, garage, zolder := newBatteryRig(t, 60, nil)
	garage.set(60, 800)
	zolder.set(60, 1200)

	r.cycles(6, 30*time.Second)
	r.assertCurrent(capCeilingCurrent, "ceiling without the battery power")
	assert.Equal(t, capCeilingCurrent, r.maxCommanded(), "never raised by the battery power")
	assert.Empty(t, garage.sent(), "no mode sent")
	assert.Empty(t, zolder.sent())

	// oven: the house alone is above the budget, the batteries cover it, the vehicle gets nothing extra
	r.inst.set(func(inst *capInstallation) { inst.house = 3000 })
	r.cycles(4, 30*time.Second)
	current, enabled := r.inst.state()
	assert.True(t, !enabled || current <= 6, "paused or at most the minimum current, got %.1fA", current)
}
