package core

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/cenkalti/backoff/v4"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/capacity"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/core/metrics"
	"github.com/evcc-io/evcc/core/prioritizer"
	"github.com/evcc-io/evcc/db"
	"github.com/evcc-io/evcc/messenger"
	"github.com/evcc-io/evcc/tariff"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Contract tests: the real control cycle Site.update (meters, slot budget, loadpoint
// update, setLimit) against a simulated single phase installation. Assertions are on
// what the charger receives.
//
// Numbers used throughout: 230V, 6A = 1380W, 32A = 7360W, floor 2500W, margin 150W,
// target 2350W. Early in a slot the remaining budget is the target, so the charger
// ceiling is its own power plus 2350W minus grid power. With the house at 500W and the
// charger at 6A: 1380 + 2350 - 1880 = 1850W = 8.04A, rounded down to 8.0A.

// capRigStart is a Tuesday morning, outside the cooking windows
var capRigStart = time.Date(2026, 10, 20, 10, 0, 0, 0, capacity.Location)

const capCeilingCurrent = 8.0 // A, see above

// capInstallation simulates house load, pv and an Alfen-like charger with 0.1A steps
// whose charge power follows the offered current while enabled
type capInstallation struct {
	mu sync.Mutex

	house, pv float64 // W
	gridErr   error   // grid meter read error

	connected, enabled bool
	current            float64 // A offered
	features           []api.Feature

	currents []float64 // current commands in order
	enables  []bool    // enable commands in order
}

func (inst *capInstallation) chargePower() float64 {
	if inst.connected && inst.enabled {
		return inst.current * 230
	}
	return 0
}

func (inst *capInstallation) set(fun func(inst *capInstallation)) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	fun(inst)
}

func (inst *capInstallation) state() (current float64, enabled bool) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.current, inst.enabled
}

func (inst *capInstallation) commands() ([]float64, []bool) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return slices.Clone(inst.currents), slices.Clone(inst.enables)
}

type capCharger struct{ *capInstallation }

func (c *capCharger) Status() (api.ChargeStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case !c.connected:
		return api.StatusA, nil
	case c.enabled:
		return api.StatusC, nil
	default:
		return api.StatusB, nil
	}
}

func (c *capCharger) Enabled() (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enabled, nil
}

func (c *capCharger) Enable(enable bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enabled = enable
	c.enables = append(c.enables, enable)
	return nil
}

func (c *capCharger) MaxCurrent(current int64) error {
	return c.MaxCurrentMillis(float64(current))
}

func (c *capCharger) MaxCurrentMillis(current float64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = current
	c.currents = append(c.currents, current)
	return nil
}

func (c *capCharger) CurrentPower() (float64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chargePower(), nil
}

func (c *capCharger) Features() []api.Feature {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.features
}

type capGridMeter struct{ *capInstallation }

func (m *capGridMeter) CurrentPower() (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gridErr != nil {
		// permanent: no retries within the cycle
		return 0, backoff.Permanent(m.gridErr)
	}
	return m.house + m.chargePower() - m.pv, nil
}

type capPvMeter struct{ *capInstallation }

func (m *capPvMeter) CurrentPower() (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pv, nil
}

// capRig runs Site.update with a mock clock shared by site, collectors and loadpoint
type capRig struct {
	t    *testing.T
	clk  *clock.Mock
	inst *capInstallation
	site *Site
	lp   *Loadpoint

	ui   chan util.Param
	push chan messenger.Event
	lpv  map[string]any // last published loadpoint values
}

// newCapRig creates the rig. Setup runs before the capacity mode is set and the
// loadpoint is prepared, so it can change the installation, site and loadpoint.
func newCapRig(t *testing.T, mode string, setup func(r *capRig)) *capRig {
	t.Helper()

	require.NoError(t, db.NewInstance("sqlite", ":memory:"))
	require.NoError(t, metrics.SetupSchema())

	clk := clock.NewMock()
	clk.Set(capRigStart)

	inst := &capInstallation{house: 500, connected: true, enabled: true}

	site := NewSite()
	site.log = util.NewLogger("site")
	site.prioritizer = prioritizer.New(site.log)
	site.stats = NewStats()
	site.stats.updated = time.Now() // no session statistics
	site.tariffs = new(tariff.Tariffs)
	site.capacityCtl.clock = clk

	for _, c := range []struct{ group, name string }{
		{metrics.Home, metrics.Home},
		{metrics.Grid, "grid"},
		{metrics.PV, "pv"},
	} {
		col, err := metrics.NewCollector(c.group, c.name, c.name, metrics.WithClock(clk))
		require.NoError(t, err)
		site.collectors[c.name] = col
	}

	site.Meters.GridMeterRef = "grid"
	site.Meters.PVMetersRef = []string{"pv"}
	site.gridMeter = config.NewStaticDevice(config.Named{Name: "grid"}, api.Meter(&capGridMeter{inst}))
	site.pvMeters = []config.Device[api.Meter]{config.NewStaticDevice(config.Named{Name: "pv"}, api.Meter(&capPvMeter{inst}))}

	charger := &capCharger{inst}

	lp := NewLoadpoint(util.NewLogger("lp"), nil)
	lp.clock = clk
	lp.charger = charger
	lp.chargeMeter = newChargeMeter(charger)
	lp.chargeRater = &Null{}
	lp.chargeTimer = &Null{}
	lp.wakeUpTimer = NewTimer()
	lp.minCurrent, lp.maxCurrent = 6, 32
	lp.phases, lp.phasesConfigured = 1, 1
	lp.status = api.StatusC
	lp.mode = api.ModeNow
	lp.capacity = site

	r := &capRig{
		t:    t,
		clk:  clk,
		inst: inst,
		site: site,
		lp:   lp,
		ui:   make(chan util.Param, 1<<12),
		push: make(chan messenger.Event, 1<<10),
		lpv:  make(map[string]any),
	}

	if setup != nil {
		setup(r)
	}

	if mode != CapacityOff {
		require.NoError(t, site.SetCapacityMode(mode))
	}

	site.loadpoints = []*Loadpoint{lp}
	lp.Prepare(site, r.ui, r.push, make(chan *Loadpoint, 1))
	r.drain()

	return r
}

// drain collects the values the loadpoint published
func (r *capRig) drain() {
	for {
		select {
		case p := <-r.ui:
			r.lpv[p.Key] = p.Val
		case <-r.push:
		default:
			return
		}
	}
}

// cycle advances the clock by d and runs one control cycle
func (r *capRig) cycle(d time.Duration) {
	r.t.Helper()
	r.clk.Add(d)
	r.site.update(r.lp)
	r.drain()
}

// cycles runs n control cycles d apart
func (r *capRig) cycles(n int, d time.Duration) {
	r.t.Helper()
	for range n {
		r.cycle(d)
	}
}

func (r *capRig) assertCurrent(expected float64, msg string) {
	r.t.Helper()
	current, enabled := r.inst.state()
	assert.True(r.t, enabled, "%s: charger enabled", msg)
	assert.InDelta(r.t, expected, current, 1e-9, "%s: charger current", msg)
}

func (r *capRig) assertDisabled(msg string) {
	r.t.Helper()
	_, enabled := r.inst.state()
	assert.False(r.t, enabled, "%s: charger disabled", msg)
}

// maxCommanded returns the highest current sent to the charger
func (r *capRig) maxCommanded() float64 {
	currents, _ := r.inst.commands()
	if len(currents) == 0 {
		return 0
	}
	return slices.Max(currents)
}

func (r *capRig) assertPublished(key string, expected any) {
	r.t.Helper()
	assert.Equal(r.t, expected, r.lpv[key], "published %s", key)
}

// Fast charging is capped to the budget: 8.0A instead of 32A, for the whole slot
func TestCapacityContractFast(t *testing.T) {
	r := newCapRig(t, CapacityOn, nil)

	// no budget before the first measurement: minimum current, never 32A
	r.assertCurrent(6, "prepare")

	r.cycle(30 * time.Second)
	r.assertCurrent(capCeilingCurrent, "first cycle")
	r.assertPublished(keys.CapacityLimited, true)
	r.assertPublished(keys.CapacityLimitReason, string(capacity.ReasonBudget))
	r.assertPublished(keys.CapacityEnforced, true)
	assert.InDelta(t, 1850, r.lpv[keys.CapacityMaxPower], 1e-6)

	st := r.site.capacityStatus()
	assert.Equal(t, CapacityOn, st.Mode)
	assert.Equal(t, 2350.0, st.Target)
	assert.Equal(t, 2350.0, st.Budget)
	assert.True(t, st.SlotStart.Equal(capRigStart))

	// grid import 500 + 1840 = 2340W stays below target, so the ceiling holds at 1850W
	r.cycles(20, 30*time.Second)
	r.assertCurrent(capCeilingCurrent, "after 10 minutes")
	assert.Equal(t, capCeilingCurrent, r.maxCommanded(), "never above the ceiling")
	assert.InDelta(t, 2340, r.site.capacityStatus().SlotAverage, 30, "slot average below target")
}

// A charging plan charges right away like fast mode, within the budget
func TestCapacityContractPlan(t *testing.T) {
	r := newCapRig(t, CapacityOn, func(r *capRig) {
		r.lp.mode = api.ModeSmart
		r.lp.planEnergy = 20 // kWh
		r.lp.planTime = capRigStart.Add(10 * time.Hour)
	})

	r.cycles(4, 30*time.Second)
	assert.True(t, r.lp.planActive, "plan active")
	r.assertCurrent(capCeilingCurrent, "plan")
	assert.Equal(t, capCeilingCurrent, r.maxCommanded(), "never above the ceiling")
	r.assertPublished(keys.CapacityLimited, true)
}

// Charging to the minimum soc is capped as well
func TestCapacityContractMinSoc(t *testing.T) {
	r := newCapRig(t, CapacityOn, func(r *capRig) {
		r.lp.mode = api.ModeSmart
		r.lp.minSoc = 50
		r.lp.vehicleSoc = 20
	})

	r.cycles(4, 30*time.Second)
	assert.True(t, r.lp.minSocNotReached(), "min soc charging")
	r.assertCurrent(capCeilingCurrent, "min soc")
	assert.Equal(t, capCeilingCurrent, r.maxCommanded(), "never above the ceiling")
}

// Solar surplus adds to the ceiling by itself: pv mode is not capped at the budget.
// House 500W, pv 6000W: the charger goes from 6A to (6000 - 500) / 230 = 23.9A.
func TestCapacityContractPvSurplus(t *testing.T) {
	r := newCapRig(t, CapacityOn, func(r *capRig) {
		r.lp.mode = api.ModeSmart
		r.inst.pv = 6000
	})

	r.cycles(4, 30*time.Second)

	current, enabled := r.inst.state()
	assert.True(t, enabled)
	assert.InDelta(t, 5500.0/230, current, 1e-6, "pv surplus current")
	assert.Greater(t, current, 2350.0/230, "above what the budget alone allows")
	r.assertPublished(keys.CapacityLimited, false)
	assert.InDelta(t, 7360, r.lpv[keys.CapacityMaxPower], 1e-6, "ceiling at maximum power")
}

// Grid charging at minimum current (smart with always charge) pauses when the house
// pushes the slot above target, and restarts after 5 minutes once there is room again.
// House 1500W + 1380W = 2880W > 2350W. After the house drops to 300W the headroom is
// 2350 - 300 = 2050W >= 1380 + 300W.
func TestCapacityContractAlwaysChargePause(t *testing.T) {
	r := newCapRig(t, CapacityOn, func(r *capRig) {
		r.lp.mode = api.ModeSmart
		r.lp.alwaysCharge = api.AlwaysChargeOn
		r.inst.house = 1500
	})

	r.cycle(30 * time.Second) // 10:00:30
	r.assertDisabled("paused")
	r.assertPublished(keys.CapacityLimitReason, string(capacity.ReasonBudget))

	r.inst.set(func(inst *capInstallation) { inst.house = 300 })

	r.cycles(9, 30*time.Second) // 10:05:00, pause 4:30
	r.assertDisabled("restart pause")
	r.assertPublished(keys.CapacityLimitReason, string(capacity.ReasonPaused))

	r.cycle(30 * time.Second) // 10:05:30, pause 5:00, headroom 4:30
	r.assertCurrent(6, "restarted")

	_, enables := r.inst.commands()
	assert.Equal(t, []bool{false, true}, enables, "one pause, one restart")
}

// Welcome charge at minimum current. In smart mode it obeys the budget, in mode off
// the capacity limiter treats the loadpoint as idle and does not limit it.
func TestCapacityContractWelcome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    api.ChargeMode
		house   float64
		enabled bool
	}{
		{"smart, budget available", api.ModeSmart, 500, true},
		// 0 + 2350 - 3000 < 1380W: no room for minimum current
		{"smart, budget exhausted", api.ModeSmart, 3000, false},
		// unlimited, bounded by the 60s welcome at minimum current: at most
		// 1380W x 60s = 23Wh, 92W of the slot average
		{"off, budget exhausted", api.ModeOff, 3000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCapRig(t, CapacityOn, func(r *capRig) {
				r.lp.mode = tc.mode
				r.lp.status = api.StatusA
				r.inst.house = tc.house
				r.inst.connected, r.inst.enabled = false, false
				r.inst.features = []api.Feature{api.WelcomeCharge}
			})

			r.cycle(30 * time.Second)
			r.assertDisabled("disconnected")

			r.inst.set(func(inst *capInstallation) { inst.connected = true })
			r.cycle(30 * time.Second)
			require.True(t, r.lp.welcomeActive())

			if tc.enabled {
				r.assertCurrent(6, "welcome charge")
			} else {
				r.assertDisabled("welcome charge")
				r.assertPublished(keys.CapacityLimited, true)
			}
		})
	}
}

// Shadow mode sends the charger exactly what mode off sends, while the capacity status
// and the loadpoint decision are still computed and published
func TestCapacityContractShadow(t *testing.T) {
	run := func(mode string) *capRig {
		r := newCapRig(t, mode, nil)
		r.cycles(2, 30*time.Second)
		return r
	}

	off := run(CapacityOff)
	shadow := run(CapacityShadow)

	// off: fast charges at 32A
	off.assertCurrent(32, "off")
	offCurrents, offEnables := off.inst.commands()
	assert.Equal(t, []float64{6, 32}, offCurrents, "off: prepare at minimum, then fast")
	off.assertPublished(keys.CapacityLimited, false)
	assert.Equal(t, capacityStatus{Mode: CapacityOff, Target: 2350}, off.site.capacityStatus(), "off: no budget")

	shadowCurrents, shadowEnables := shadow.inst.commands()
	assert.Equal(t, offCurrents, shadowCurrents, "shadow: same current commands as off")
	assert.Equal(t, offEnables, shadowEnables, "shadow: same enable commands as off")

	// what it would have done: 7360 + 2350 - 7860 = 1850W
	shadow.assertPublished(keys.CapacityLimited, true)
	shadow.assertPublished(keys.CapacityEnforced, false)
	assert.InDelta(t, 1850, shadow.lpv[keys.CapacityMaxPower], 1e-6)

	st := shadow.site.capacityStatus()
	assert.Equal(t, CapacityShadow, st.Mode)
	assert.Equal(t, 2350.0, st.Budget)
	assert.True(t, st.SlotStart.Equal(capRigStart))
	assert.Greater(t, st.SlotEnergy, 0.0)
}

// A grid meter read error skips the loadpoint update. The last limit stays until the
// meter timeout of 30s, then the fallback drops the charger to minimum current.
func TestCapacityContractGridMeterError(t *testing.T) {
	r := newCapRig(t, CapacityOn, nil)

	r.cycles(2, 10*time.Second) // 10:00:20, last measurement
	r.assertCurrent(capCeilingCurrent, "measured")
	currents, _ := r.inst.commands()

	r.inst.set(func(inst *capInstallation) { inst.gridErr = errors.New("timeout") })

	r.cycles(2, 10*time.Second) // 10:00:40, 20s without measurement
	r.assertCurrent(capCeilingCurrent, "before the meter timeout")
	after, _ := r.inst.commands()
	assert.Equal(t, currents, after, "no charger commands before the meter timeout")
	assert.False(t, r.site.capacityStatus().MeterFallback)

	r.cycle(10 * time.Second) // 10:00:50, 30s
	r.assertCurrent(6, "meter timeout")
	assert.True(t, r.site.capacityStatus().MeterFallback)
	r.assertPublished(keys.CapacityLimitReason, string(capacity.ReasonMeter))

	r.cycle(10 * time.Second) // still failing
	r.assertCurrent(6, "meter still down")

	r.inst.set(func(inst *capInstallation) { inst.gridErr = nil })

	r.cycle(10 * time.Second) // 10:01:10
	r.assertCurrent(capCeilingCurrent, "meter restored")
	assert.False(t, r.site.capacityStatus().MeterFallback)
	assert.Equal(t, capCeilingCurrent, r.maxCommanded(), "never above the ceiling")
}

// With the default 30s interval the previous measurement is already 30s old at the first
// failed cycle, so a single failed read falls back to minimum current
func TestCapacityContractGridMeterErrorInterval(t *testing.T) {
	r := newCapRig(t, CapacityOn, nil)

	r.cycle(30 * time.Second)
	r.assertCurrent(capCeilingCurrent, "measured")

	r.inst.set(func(inst *capInstallation) { inst.gridErr = errors.New("timeout") })
	r.cycle(30 * time.Second)
	r.assertCurrent(6, "first failed read")
	assert.True(t, r.site.capacityStatus().MeterFallback)
}

// A configured grid meter that failed to start: mode on stays on and the charger stays
// at minimum current
func TestCapacityContractGridMeterMissing(t *testing.T) {
	r := newCapRig(t, CapacityOn, func(r *capRig) {
		r.site.gridMeter = nil
	})

	assert.Equal(t, CapacityOn, r.site.GetCapacityMode())
	r.assertCurrent(6, "prepare")

	r.cycle(10 * time.Second)
	r.assertCurrent(6, "before the meter timeout")
	assert.False(t, r.site.capacityStatus().MeterFallback)

	r.cycles(4, 30*time.Second)
	r.assertCurrent(6, "after the meter timeout")
	assert.True(t, r.site.capacityStatus().MeterFallback)
	assert.Equal(t, CapacityOn, r.site.GetCapacityMode())

	currents, _ := r.inst.commands()
	assert.Equal(t, []float64{6}, currents, "minimum current only")
}

// The slot energy used by the budget restarts at the quarter boundary. Import up to the
// first reading after the boundary is booked to the completed slot, the new slot assumes
// import at target until its first reading.
func TestCapacityContractSlotBoundary(t *testing.T) {
	r := newCapRig(t, CapacityOn, func(r *capRig) {
		r.lp.mode = api.ModeOff
		r.inst.house = 2600
	})

	kWh := func(power float64, d time.Duration) float64 { return power * d.Hours() / 1e3 }

	r.cycle(0) // 10:00:00, first reading on the boundary
	r.assertDisabled("mode off")
	assert.Equal(t, 0.0, r.site.capacityStatus().SlotAssumed)

	r.cycles(29, 30*time.Second) // 10:14:30
	st := r.site.capacityStatus()
	assert.True(t, st.SlotStart.Equal(capRigStart))
	assert.InDelta(t, kWh(2600, 870*time.Second), st.SlotEnergy, 1e-9)
	assert.InDelta(t, 2600, st.SlotAverage, 1e-6)
	assert.Equal(t, 0.0, st.Budget, "budget used up")
	assert.True(t, st.Exceeded)
	assert.Equal(t, 2350.0, st.Target)

	r.cycle(30 * time.Second) // 10:15:00
	st = r.site.capacityStatus()
	assert.True(t, st.SlotStart.Equal(capRigStart.Add(15*time.Minute)), "new slot")
	assert.Equal(t, 0.0, st.SlotEnergy, "slot energy restarts")
	assert.Equal(t, 0.0, st.SlotAssumed)
	assert.False(t, st.Exceeded)

	// the completed slot of 2600W is the month peak of the own count and raises the target
	assert.Equal(t, "evcc", st.MonthPeakSrc)
	assert.InDelta(t, 2600, st.MonthPeakEvcc, 1e-6)
	assert.InDelta(t, 2450, st.Target, 1e-6)
	assert.InDelta(t, 2450, st.Budget, 1e-6)

	// next slot: first reading 20s after the boundary
	r.inst.set(func(inst *capInstallation) { inst.house = 1000 })
	r.cycles(29, 30*time.Second) // 10:29:30
	r.cycle(50 * time.Second)    // 10:30:20

	st = r.site.capacityStatus()
	assert.True(t, st.SlotStart.Equal(capRigStart.Add(30*time.Minute)), "new slot")
	assert.InDelta(t, kWh(2450, 20*time.Second), st.SlotAssumed, 1e-9, "assumed at target until the first reading")
	assert.InDelta(t, st.SlotAssumed, st.SlotEnergy, 1e-12, "nothing counted yet")
	assert.InDelta(t, 2450, st.Budget, 1e-6)
	assert.InDelta(t, 2600, st.MonthPeakEvcc, 1e-6, "the 1000W slot does not change the month peak")
}
