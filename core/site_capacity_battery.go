package core

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/capacity"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/core/metrics"
	"github.com/evcc-io/evcc/core/site"
	"github.com/evcc-io/evcc/db/settings"
	"github.com/evcc-io/evcc/util/config"
	"github.com/jinzhu/now"
)

// capacityBatteryPowerPerKWh estimates the charge and discharge power of a battery without a
// configured power: 200W per kWh, 800W for 4 kWh and 1200W for 6 kWh as the Indevolt PowerFlex
const capacityBatteryPowerPerKWh = 200.0

// capacityBatteryMaxPower bounds a configured battery power
const capacityBatteryMaxPower = 10000.0

// capacityBattery is the battery control state of the capacity tariff
type capacityBattery struct {
	mode       string // setting: off, shadow, on
	gridCharge bool   // grid charging on forecast
	powers     map[string]float64
	reserve    capacity.ReserveSettings

	ctl      *capacity.BatteryController
	decision *capacity.BatteryDecision // last decision, nil before the first
	units    capacity.BatteryUnits     // units of the last decision
	vehicle  capacity.VehicleState
	resend   []string // units whose mode the watchdog wants sent again

	plan      capacity.ReservePlan
	planSlot  time.Time // slot the plan was made in, zero to replan
	planUnits string    // units read when the plan was made
	planErr   string    // last profile error logged

	known      map[string]capacity.BatteryUnit // last reading per unit, for a unit not read this cycle
	overCharge map[string]time.Time            // slot a unit was logged charging above its power

	effective string                           // effective mode of the last cycle
	logged    map[string]capacity.UnitDecision // last logged decision per unit
	assumed   map[string]bool                  // units logged with an estimated power
}

func newCapacityBattery() capacityBattery {
	return capacityBattery{
		mode:       CapacityOff,
		powers:     make(map[string]float64),
		reserve:    capacity.DefaultReserveSettings(),
		ctl:        capacity.NewBatteryController(capacity.DefaultBatterySettings()),
		logged:     make(map[string]capacity.UnitDecision),
		assumed:    make(map[string]bool),
		known:      make(map[string]capacity.BatteryUnit),
		overCharge: make(map[string]time.Time),
	}
}

// resetControl forgets the decisions and the rule state, e.g. after a mode change
func (b *capacityBattery) resetControl() {
	b.ctl.Reset()
	b.decision = nil
	b.resend = nil
	b.planSlot = time.Time{}
	b.logged = make(map[string]capacity.UnitDecision)
}

var _ site.CapacityBatteryAPI = (*Site)(nil)

// capacityBatteryUnitStatus is one battery in the published state
type capacityBatteryUnitStatus struct {
	Name     string               `json:"name"`
	Title    string               `json:"title"`
	Soc      float64              `json:"soc"`
	Energy   float64              `json:"energy"`   // kWh
	Capacity float64              `json:"capacity"` // kWh
	Power    float64              `json:"power"`    // W, positive discharging
	MaxPower float64              `json:"maxPower"` // W used for the rules
	Assumed  bool                 `json:"assumed"`  // maxPower estimated from the capacity
	Mode     capacity.BatteryMode `json:"mode"`
	Rule     capacity.BatteryRule `json:"rule"`
	Reserve  float64              `json:"reserve"` // kWh share of the reserve target
	Floor    float64              `json:"floor"`   // kWh a charging vehicle may use the unit down to
	Failed   bool                 `json:"failed"`  // fell back to normal in the watchdog until restart
}

// capacityBatteryStatus is published as keys.CapacityBattery
type capacityBatteryStatus struct {
	Mode       string                      `json:"mode"` // effective: off, shadow or on
	Energy     float64                     `json:"energy"`
	Capacity   float64                     `json:"capacity"`
	Reserve    float64                     `json:"reserve"`
	Available  float64                     `json:"available"` // W the units in normal can deliver
	FastFloor  float64                     `json:"fastFloor"` // kWh fast charging may use the battery down to
	GridCharge bool                        `json:"gridCharge"`
	Units      []capacityBatteryUnitStatus `json:"units"`
	Plan       *capacity.ReservePlan       `json:"plan"` // null before the first plan
}

// effectiveBatteryMode combines the capacity mode with the battery mode: on only if both are on
func effectiveBatteryMode(capacityMode, batteryMode string) string {
	switch {
	case capacityMode == CapacityOff || batteryMode == CapacityOff:
		return CapacityOff
	case capacityMode == CapacityShadow || batteryMode == CapacityShadow:
		return CapacityShadow
	default:
		return CapacityOn
	}
}

// restoreCapacityBatterySettings restores the persisted battery settings
func (site *Site) restoreCapacityBatterySettings() {
	if site.capacityCtl == nil {
		return
	}

	var powers map[string]float64
	if err := settings.Json(keys.CapacityBatteryPowers, &powers); err == nil {
		for name, p := range powers {
			if err := site.SetCapacityBatteryPower(name, p); err != nil {
				site.log.WARN.Printf("capacity battery power: %v", err)
			}
		}
	}
	if v, err := settings.String(keys.CapacityFastBattery); err == nil && v != "" {
		if err := site.SetCapacityFastBattery(v); err != nil {
			site.log.WARN.Printf("capacity fast battery: %v", err)
		}
	}
	if v, err := settings.String(keys.CapacityReserveMode); err == nil && v != "" {
		if err := site.SetCapacityReserveMode(v); err != nil {
			site.log.WARN.Printf("capacity reserve mode: %v", err)
		}
	}
	if v, err := settings.Bool(keys.CapacityGridCharge); err == nil {
		if err := site.SetCapacityGridCharge(v); err != nil {
			site.log.WARN.Printf("capacity grid charge: %v", err)
		}
	}
	if v, err := settings.String(keys.CapacityBatteryMode); err == nil && v != "" {
		if err := site.SetCapacityBatteryMode(v); err != nil {
			site.log.WARN.Printf("capacity battery mode: %v", err)
		}
	}

	site.publish(keys.CapacityBatteryMode, site.GetCapacityBatteryMode())
	site.publish(keys.CapacityFastBattery, site.GetCapacityFastBattery())
	site.publish(keys.CapacityReserveMode, site.GetCapacityReserveMode())
	site.publish(keys.CapacityGridCharge, site.GetCapacityGridCharge())
	site.publish(keys.CapacityBatteryPowers, site.GetCapacityBatteryPowers())
}

// capacityBatteryGet reads the capacity control under its lock, def without capacity control
func capacityBatteryGet[T any](site *Site, def T, get func(c *siteCapacity) T) T {
	c := site.capacityCtl
	if c == nil {
		return def
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return get(c)
}

// capacityBatterySet changes the battery state under the lock and reports whether set changed it
func (site *Site) capacityBatterySet(set func(b *capacityBattery) bool) (bool, error) {
	c := site.capacityCtl
	if c == nil {
		return false, errors.New("capacity control not available")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return set(&c.bat), nil
}

// GetCapacityBatteryMode returns the battery control mode: off, shadow or on
func (site *Site) GetCapacityBatteryMode() string {
	return capacityBatteryGet(site, CapacityOff, func(c *siteCapacity) string { return c.bat.mode })
}

// SetCapacityBatteryMode sets the battery control mode. It acts only while the capacity mode
// is not off, and controls the batteries only when both are on.
func (site *Site) SetCapacityBatteryMode(mode string) error {
	if !slices.Contains(CapacityModes, mode) {
		return fmt.Errorf("invalid capacity battery mode: %s", mode)
	}
	if mode != CapacityOff && !site.hasBatteryControl() {
		return fmt.Errorf("capacity battery mode %s requires a controllable battery", mode)
	}

	changed, err := site.capacityBatterySet(func(b *capacityBattery) bool {
		if b.mode == mode {
			return false
		}
		b.mode = mode
		b.resetControl()
		return true
	})
	if err != nil || !changed {
		return err
	}

	site.log.INFO.Printf("capacity battery mode: %s", mode)
	settings.SetString(keys.CapacityBatteryMode, mode)
	site.publish(keys.CapacityBatteryMode, mode)
	site.publish(keys.CapacityBattery, site.capacityBatteryStatus())

	return nil
}

// GetCapacityFastBattery returns how far the battery may support fast charging
func (site *Site) GetCapacityFastBattery() string {
	return capacityBatteryGet(site, "", func(c *siteCapacity) string { return string(c.bat.ctl.Settings().FastBattery) })
}

// SetCapacityFastBattery sets how far the battery may support fast charging: buffer or reserve
func (site *Site) SetCapacityFastBattery(v string) error {
	fb := capacity.FastBattery(v)
	if !slices.Contains(capacity.FastBatteries, fb) {
		return fmt.Errorf("invalid capacity fast battery: %s", v)
	}

	changed, err := site.capacityBatterySet(func(b *capacityBattery) bool {
		s := b.ctl.Settings()
		if s.FastBattery == fb {
			return false
		}
		s.FastBattery = fb
		b.ctl.SetSettings(s)
		return true
	})
	if err != nil || !changed {
		return err
	}

	settings.SetString(keys.CapacityFastBattery, v)
	site.publish(keys.CapacityFastBattery, v)

	return nil
}

// GetCapacityReserveMode returns the reserve mode: dynamic or fixed
func (site *Site) GetCapacityReserveMode() string {
	return capacityBatteryGet(site, "", func(c *siteCapacity) string { return string(c.bat.reserve.Mode) })
}

// SetCapacityReserveMode sets the reserve mode: dynamic or fixed
func (site *Site) SetCapacityReserveMode(v string) error {
	mode := capacity.ReserveMode(v)
	if !slices.Contains(capacity.ReserveModes, mode) {
		return fmt.Errorf("invalid capacity reserve mode: %s", v)
	}

	changed, err := site.capacityBatterySet(func(b *capacityBattery) bool {
		changed := b.reserve.Mode != mode
		b.reserve.Mode = mode
		b.planSlot = time.Time{}
		return changed
	})
	if err != nil || !changed {
		return err
	}

	settings.SetString(keys.CapacityReserveMode, v)
	site.publish(keys.CapacityReserveMode, v)

	return nil
}

// GetCapacityGridCharge returns whether the batteries charge from the grid on forecast
func (site *Site) GetCapacityGridCharge() bool {
	return capacityBatteryGet(site, false, func(c *siteCapacity) bool { return c.bat.gridCharge })
}

// SetCapacityGridCharge sets whether the batteries charge from the grid on forecast
func (site *Site) SetCapacityGridCharge(enable bool) error {
	changed, err := site.capacityBatterySet(func(b *capacityBattery) bool {
		changed := b.gridCharge != enable
		b.gridCharge = enable
		return changed
	})
	if err != nil || !changed {
		return err
	}

	settings.SetBool(keys.CapacityGridCharge, enable)
	site.publish(keys.CapacityGridCharge, enable)

	return nil
}

// GetCapacityBatteryPowers returns the configured charge and discharge power per battery in W
func (site *Site) GetCapacityBatteryPowers() map[string]float64 {
	return capacityBatteryGet(site, nil, func(c *siteCapacity) map[string]float64 { return maps.Clone(c.bat.powers) })
}

// SetCapacityBatteryPower sets the charge and discharge power of a battery in W, 0 to estimate it
// from the capacity. The battery is named by its device name or title.
func (site *Site) SetCapacityBatteryPower(name string, power float64) error {
	if math.IsNaN(power) || power < 0 || power > capacityBatteryMaxPower {
		return fmt.Errorf("invalid capacity battery power: %v, must be 0 to %.0fW", power, capacityBatteryMaxPower)
	}

	dev, ok := site.capacityBatteryDevice(name)
	if !ok {
		return fmt.Errorf("unknown battery: %s", name)
	}
	name = dev.Config().Name

	var powers map[string]float64
	changed, err := site.capacityBatterySet(func(b *capacityBattery) bool {
		changed := b.powers[name] != power
		if power == 0 {
			delete(b.powers, name)
		} else {
			b.powers[name] = power
		}
		delete(b.assumed, name)
		b.planSlot = time.Time{}
		powers = maps.Clone(b.powers)
		return changed
	})
	if err != nil || !changed {
		return err
	}

	if err := settings.SetJson(keys.CapacityBatteryPowers, powers); err != nil {
		return err
	}
	site.publish(keys.CapacityBatteryPowers, powers)

	return nil
}

// capacityBatteryDevice finds a battery by device name or title
func (site *Site) capacityBatteryDevice(name string) (config.Device[api.Meter], bool) {
	for _, dev := range site.batteryMeters {
		if dev.Config().Name == name || deviceTitleOrName(dev) == name {
			return dev, true
		}
	}
	return nil, false
}

// capacityBatteryEffective returns the effective battery control mode
func (site *Site) capacityBatteryEffective() string {
	return capacityBatteryGet(site, CapacityOff, func(c *siteCapacity) string { return effectiveBatteryMode(c.mode, c.bat.mode) })
}

// capacityBatteryUnits returns the controllable batteries as measured this cycle. A battery
// without soc or capacity counts as not read, with its last known soc and capacity, so it keeps
// its share of the reserve.
func (site *Site) capacityBatteryUnits() capacity.BatteryUnits {
	devices := site.state().battery.Devices

	c := site.capacityCtl
	c.mu.Lock()
	defer c.mu.Unlock()
	b := &c.bat

	var res capacity.BatteryUnits
	for i, dev := range site.batteryMeters {
		if !api.HasCap[api.BatteryController](dev.Instance()) {
			continue
		}

		name := dev.Config().Name
		u := capacity.BatteryUnit{Name: name, MaxSoc: 100}
		if i < len(devices) {
			m := devices[i]
			u.Power = m.Power
			if m.Soc != nil && m.Capacity != nil && *m.Capacity > 0 {
				u.Reachable, u.Soc, u.Capacity = true, *m.Soc, *m.Capacity
			}
			if m.MinSoc != nil {
				u.MinSoc = *m.MinSoc
			}
			if m.MaxSoc != nil && *m.MaxSoc > 0 {
				u.MaxSoc = *m.MaxSoc
			}
		}

		if u.Reachable && !math.IsNaN(u.Soc) {
			b.known[name] = u
		} else if k, ok := b.known[name]; ok {
			u = k
			u.Reachable, u.Power = false, 0
		}

		u.MaxPower = b.powers[name]
		if u.MaxPower == 0 {
			u.MaxPower = u.Capacity * capacityBatteryPowerPerKWh
			if u.Capacity > 0 && !b.assumed[name] {
				b.assumed[name] = true
				site.log.INFO.Printf("capacity: battery %s assumed at %.0fW (%.1f kWh), set its power for exact rules", deviceTitleOrName(dev), u.MaxPower, u.Capacity)
			}
		}

		res = append(res, u)
	}

	return res
}

// capacityVehicleState returns whether a vehicle charges, or is connected and wants to charge
// regardless of solar (fast, plan, minimum soc), and whether fast. A vehicle paused by the
// capacity limit counts as well, so grid charging does not keep it from starting.
func (site *Site) capacityVehicleState() capacity.VehicleState {
	var res capacity.VehicleState
	for _, lp := range site.activeLoadpoints() {
		fast := lp.IsFastChargingActive()
		if lp.charging() || fast && lp.connected() && lp.GetMode() != api.ModeOff {
			res.Charging = true
			res.Fast = res.Fast || fast
		}
	}
	return res
}

// profileSeries maps a 96 slot day profile in kWh to W per slot for n slots from the slot
// containing at. The profile is indexed by local wall clock as its query groups it (SQLite
// localtime, the process time zone), so looking up each slot by its wall clock keeps the
// profile in place on days with 92 or 100 slots.
func profileSeries(profile *[96]float64, at time.Time, n int, loc *time.Location) []float64 {
	res := make([]float64, n)
	slot := capacity.SlotStart(at)
	for i := range res {
		l := slot.Add(time.Duration(i) * capacity.SlotDuration).In(loc)
		res[i] = capacity.SlotPower(profile[(l.Hour()*60+l.Minute())/15])
	}
	return res
}

// capacityHouseProfiles returns the house profile at the 90th and 50th percentile in W per
// slot for n slots from at, and the days of history behind it
func (site *Site) capacityHouseProfiles(at time.Time, n int) (p90, p50 []float64, days int, err error) {
	col, ok := site.collectors[metrics.Home]
	if !ok {
		return nil, nil, 0, errors.New("no home collector")
	}

	from := now.With(at.In(time.Local)).BeginningOfDay().AddDate(0, 0, -28)
	if days, err = col.ProfileDays(from); err != nil {
		return nil, nil, 0, err
	}

	for _, pc := range []struct {
		p   float64
		res *[]float64
	}{{0.9, &p90}, {0.5, &p50}} {
		profile, err := col.EnergyProfilePercentile(from, pc.p)
		if err != nil {
			return nil, nil, days, err
		}
		*pc.res = profileSeries(profile, at, n, time.Local)
	}

	return p90, p50, days, nil
}

// capacitySolar returns the solar forecast in W per slot for n slots from now, nil without forecast
func (site *Site) capacitySolar(n int, from time.Time) []float64 {
	solar, err := site.tariffRates(api.TariffUsageSolar)
	if err != nil || len(solar) == 0 {
		return nil
	}

	res := make([]float64, n)
	slot := capacity.SlotStart(from)
	for i := range res {
		start := slot.Add(time.Duration(i) * capacity.SlotDuration)
		res[i] = solarEnergy(solar, start, start.Add(capacity.SlotDuration)) / capacity.SlotDuration.Hours()
	}
	return res
}

// capacityReservePlan returns the reserve plan, made again once per slot, after a setting
// changed, and when a vehicle starts or stops charging. Profiles and forecast are read outside
// the lock. Without a profile the reserve falls back to the season floor.
func (site *Site) capacityReservePlan(at time.Time, units capacity.BatteryUnits, vehicle capacity.VehicleState) capacity.ReservePlan {
	c := site.capacityCtl
	slot := capacity.SlotStart(at)

	var read []string
	missing := false
	for _, u := range units {
		if u.Reachable {
			read = append(read, u.Name)
		} else {
			missing = true
		}
	}
	readUnits := strings.Join(read, ",")

	c.mu.Lock()
	b := &c.bat
	// a unit not read for a moment does not change the plan, it only skips commands
	fresh := b.planSlot.Equal(slot) && b.vehicle == vehicle && b.planUnits == readUnits || missing && !b.planSlot.IsZero()
	plan, rs := b.plan, b.reserve
	target := capacity.Target(c.settings, c.monthPeak.Power)
	c.mu.Unlock()

	if fresh {
		return plan
	}

	// two days cover the deadline of tomorrow
	const n = 2 * 96
	p90, p50, days, err := site.capacityHouseProfiles(at, n)
	if err != nil {
		c.mu.Lock()
		if msg := err.Error(); msg != b.planErr {
			b.planErr = msg
			site.log.DEBUG.Printf("capacity: house profile: %v, reserve from the season floor", err)
		}
		c.mu.Unlock()
		days = 0
	}

	plan = rs.PlanGridCharge(capacity.ReserveInput{
		Now:             at.In(capacity.Location),
		Units:           units,
		Target:          target,
		HouseP90:        p90,
		HouseP50:        p50,
		ProfileDays:     days,
		Solar:           site.capacitySolar(n, at),
		SolarScale:      site.effectiveSolarScale(),
		VehicleCharging: vehicle.Charging,
	})

	c.mu.Lock()
	b.plan, b.planSlot, b.vehicle, b.planUnits = plan, slot, vehicle, readUnits
	c.mu.Unlock()

	if len(plan.Slots) > 0 {
		site.log.DEBUG.Printf("capacity: reserve %.2f kWh, stored %.2f kWh, solar %.2f kWh, grid charge %.2f kWh in %d slots from %s",
			plan.Reserve, plan.Energy, plan.Surplus, plan.Need, len(plan.Slots), plan.Slots[0].Start.In(capacity.Location).Format("15:04"))
	}

	return plan
}

// capacityBatteryControls reports whether battery control sends the modes this cycle: on, no
// external mode or its return to normal pending, and the batteries put in normal after the start
func (site *Site) capacityBatteryControls() bool {
	if site.capacityBatteryEffective() != CapacityOn || site.GetBatteryModeExternal() != api.BatteryUnknown {
		return false
	}

	site.Lock()
	extReset := !site.batteryModeExternalTimer.IsZero()
	site.Unlock()
	if extReset {
		return false
	}

	c := site.capacityCtl
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.batteryStart
}

// updateCapacityBattery decides the battery modes after the slot budget, before the loadpoints.
// When the modes are sent, loadpoints then limit against grid power plus the shift the new
// modes cause, and the watchdog checks that each battery follows its mode.
func (site *Site) updateCapacityBattery() {
	c := site.capacityCtl
	if c == nil {
		return
	}

	mode := site.capacityBatteryEffective()

	c.mu.Lock()
	wasOff := c.bat.effective == CapacityOff || c.bat.effective == ""
	c.bat.effective = mode
	c.mu.Unlock()

	if mode == CapacityOff || len(site.batteryMeters) == 0 {
		if !wasOff {
			site.publish(keys.CapacityBattery, site.capacityBatteryStatus())
		}
		return
	}

	at := c.clock.Now()
	controls := site.capacityBatteryControls()
	units := site.capacityBatteryUnits()
	vehicle := site.capacityVehicleState()
	plan := site.capacityReservePlan(at, units, vehicle)

	c.mu.Lock()
	b := &c.bat
	in := capacity.BatteryInput{
		Now:   at,
		Units: units,
		// a single missed read keeps the modes, the fallback after MeterTimeout releases them
		Measured:  c.valid && !c.fallback,
		Budget:    c.budget,
		GridPower: c.gridPower,
		Threat:    c.threat,
		Vehicle:   vehicle,
		Reserve:   plan.Reserve,
	}
	if b.gridCharge {
		in.GridCharge = plan.Units(at)
	}

	d := b.ctl.Decide(in)
	if controls {
		c.gridPower += d.GridShift
		wd := b.ctl.Watchdog(in)
		b.resend = append(b.resend, wd.Resend...)
		for _, name := range wd.Resend {
			site.log.WARN.Printf("capacity: battery %s does not follow its mode, sending it again", name)
		}
		for _, name := range wd.Failed {
			site.log.ERROR.Printf("capacity: battery %s did not follow its mode after %d attempts, normal until restart", name, b.ctl.Settings().WatchdogRetries)
		}
		site.capacityCheckChargePower(d, units, at)
	}
	b.decision, b.units = &d, units
	changes := b.changesToLog(d)
	c.mu.Unlock()

	verb := "battery"
	if !controls {
		verb = "would set battery"
	}
	for _, u := range changes {
		site.log.INFO.Printf("capacity: %s %s to %s (%s)", verb, u.Name, u.Mode, u.Rule)
	}

	site.publish(keys.CapacityBattery, site.capacityBatteryStatus())
}

// changesToLog returns the unit decisions worth logging and remembers them: a new mode, or a
// new rule other than calm. A unit not read is logged only when its mode is changed for it.
func (b *capacityBattery) changesToLog(d capacity.BatteryDecision) []capacity.UnitDecision {
	var res []capacity.UnitDecision
	for _, u := range d.Units {
		prev, ok := b.logged[u.Name]
		noteworthy := !ok || prev.Mode != u.Mode || prev.Rule != u.Rule && u.Rule != capacity.RuleCalm
		if noteworthy && (u.Rule != capacity.RuleUnreachable || u.Changed) {
			b.logged[u.Name] = u
			res = append(res, u)
		}
	}
	return res
}

// capacityCheckChargePower warns once per slot when a unit charges from the grid well above the
// power the rules count with, e.g. when maxchargepower of the meter template differs. Called
// with the lock held.
func (site *Site) capacityCheckChargePower(d capacity.BatteryDecision, units capacity.BatteryUnits, at time.Time) {
	b := &site.capacityCtl.bat
	slot := capacity.SlotStart(at)

	for _, u := range units {
		ud, ok := d.Mode(u.Name)
		if !ok || ud.Mode != capacity.BatteryCharge || !u.Reachable || -u.Power <= 1.2*u.MaxPower+100 {
			continue
		}
		if b.overCharge[u.Name].Equal(slot) {
			continue
		}
		b.overCharge[u.Name] = slot
		site.log.WARN.Printf("capacity: battery %s charges at %.0fW, the rules count with %.0fW: set maxchargepower of the meter and the battery power alike", u.Name, -u.Power, u.MaxPower)
	}
}

// capacityBatteryAvailable returns the power the home battery can deliver for an appliance.
// With battery control on it is the sum of the units in normal above their minimum, else the
// site estimate of F1.
func (site *Site) capacityBatteryAvailable() float64 {
	if site.capacityBatteryEffective() != CapacityOn {
		return site.capacityBatteryAvailableSite()
	}

	return capacityBatteryGet(site, 0, func(c *siteCapacity) float64 {
		if d := c.bat.decision; d != nil {
			return d.Available
		}
		return 0
	})
}

// toBatteryMode converts a capacity battery mode
func toBatteryMode(m capacity.BatteryMode) api.BatteryMode {
	switch m {
	case capacity.BatteryHold:
		return api.BatteryHold
	case capacity.BatteryCharge:
		return api.BatteryCharge
	default:
		return api.BatteryNormal
	}
}

// capacityBatteryApply applies the decided mode to each battery when battery control is on
// and reports whether it did, so the regular battery mode control is skipped. An external
// mode, and the return to normal when it ends, are left to the regular control.
func (site *Site) capacityBatteryApply() bool {
	if site.capacityBatteryEffective() != CapacityOn {
		return false
	}

	c := site.capacityCtl
	c.mu.Lock()
	pending, d := c.batteryStart, c.bat.decision
	resend := c.bat.resend
	c.bat.resend = nil
	c.mu.Unlock()

	// the batteries get normal at start first, nothing else sends a mode meanwhile
	if pending {
		return true
	}

	if !site.capacityBatteryControls() {
		return false
	}

	if d == nil {
		return true
	}

	if site.batteryModeApplied == nil {
		site.batteryModeApplied = make(map[string]api.BatteryMode)
	}
	for _, name := range resend {
		delete(site.batteryModeApplied, name)
	}

	var errs []error
	for _, u := range d.Units {
		dev, ok := site.capacityBatteryDevice(u.Name)
		if !ok {
			continue
		}
		ctrl, ok := api.Cap[api.BatteryController](dev.Instance())
		if !ok {
			continue
		}

		// also for a unit not read: the applied mode only differs when it must leave grid charging
		mode := supportedBatteryMode(ctrl.BatteryModes(), toBatteryMode(u.Mode))
		if mode == api.BatteryUnknown || site.batteryModeApplied[u.Name] == mode {
			continue
		}

		if err := ctrl.SetBatteryMode(mode); err != nil {
			if !errors.Is(err, api.ErrNotAvailable) {
				errs = append(errs, fmt.Errorf("%s: %w", deviceTitleOrName(dev), err))
			}
			continue
		}

		site.batteryModeApplied[u.Name] = mode
		site.log.DEBUG.Printf("set battery %s mode: %s", deviceTitleOrName(dev), mode)
	}

	if err := errors.Join(errs...); err != nil {
		site.log.ERROR.Println("capacity battery mode:", err)
	}

	// the site mode shows the strongest unit mode: charge, then hold, then normal
	site.SetBatteryMode(siteBatteryMode(d))

	return true
}

// siteBatteryMode returns the site battery mode for a decision
func siteBatteryMode(d *capacity.BatteryDecision) api.BatteryMode {
	res := api.BatteryNormal
	for _, u := range d.Units {
		switch u.Mode {
		case capacity.BatteryCharge:
			return api.BatteryCharge
		case capacity.BatteryHold:
			res = api.BatteryHold
		}
	}
	return res
}

func (site *Site) capacityBatteryStatus() capacityBatteryStatus {
	titles := make(map[string]string)
	for _, dev := range site.batteryMeters {
		titles[dev.Config().Name] = deviceTitleOrName(dev)
	}

	c := site.capacityCtl
	c.mu.Lock()
	defer c.mu.Unlock()

	b := &c.bat
	res := capacityBatteryStatus{
		Mode:       effectiveBatteryMode(c.mode, b.mode),
		GridCharge: b.gridCharge,
		Units:      []capacityBatteryUnitStatus{},
	}

	if res.Mode == CapacityOff || b.decision == nil {
		return res
	}

	failed := b.ctl.Failed()
	plan := b.plan
	res.Energy, res.Capacity = b.units.Energy(), b.units.Capacity()
	res.Reserve, res.FastFloor = plan.Reserve, plan.Reserve
	res.Available = b.decision.Available
	res.Plan = &plan
	if s := b.ctl.Settings(); s.FastBattery == capacity.FastBatteryBuffer {
		res.FastFloor = math.Min(plan.Reserve, math.Max(s.Buffer, b.units.MinEnergy()))
	}

	for _, d := range b.decision.Units {
		st := capacityBatteryUnitStatus{
			Name:    d.Name,
			Title:   titles[d.Name],
			Mode:    d.Mode,
			Rule:    d.Rule,
			Reserve: d.Reserve,
			Floor:   d.Floor,
			Assumed: b.assumed[d.Name] && b.powers[d.Name] == 0,
			Failed:  slices.Contains(failed, d.Name),
		}
		if i := slices.IndexFunc(b.units, func(u capacity.BatteryUnit) bool { return u.Name == d.Name }); i >= 0 {
			u := b.units[i]
			st.Soc, st.Energy, st.Capacity, st.Power, st.MaxPower = u.Soc, u.Energy(), u.Capacity, u.Power, u.MaxPower
		}
		res.Units = append(res.Units, st)
	}

	return res
}
