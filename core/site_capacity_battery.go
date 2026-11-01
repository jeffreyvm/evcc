package core

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/capacity"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/core/metrics"
	"github.com/evcc-io/evcc/core/site"
	"github.com/evcc-io/evcc/db/settings"
	"github.com/evcc-io/evcc/util/config"
	"github.com/jinzhu/now"
	"github.com/samber/lo"
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

	plan     capacity.ReservePlan
	planSlot time.Time // slot the plan was made in, zero to replan
	planErr  string    // last profile error logged

	effective string                           // effective mode of the last cycle
	logged    map[string]capacity.UnitDecision // last logged decision per unit
	assumed   map[string]bool                  // units logged with an estimated power
}

func newCapacityBattery() capacityBattery {
	return capacityBattery{
		mode:    CapacityOff,
		powers:  make(map[string]float64),
		reserve: capacity.DefaultReserveSettings(),
		ctl:     capacity.NewBatteryController(capacity.DefaultBatterySettings()),
		logged:  make(map[string]capacity.UnitDecision),
		assumed: make(map[string]bool),
	}
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

// GetCapacityBatteryMode returns the battery control mode: off, shadow or on
func (site *Site) GetCapacityBatteryMode() string {
	c := site.capacityCtl
	if c == nil {
		return CapacityOff
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bat.mode
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

	c := site.capacityCtl
	if c == nil {
		return errors.New("capacity control not available")
	}

	c.mu.Lock()
	changed := c.bat.mode != mode
	c.bat.mode = mode
	if changed {
		c.bat.ctl.Reset()
		c.bat.decision = nil
		c.bat.planSlot = time.Time{}
		c.bat.logged = make(map[string]capacity.UnitDecision)
	}
	c.mu.Unlock()

	if changed {
		site.log.INFO.Printf("capacity battery mode: %s", mode)
		settings.SetString(keys.CapacityBatteryMode, mode)
		site.publish(keys.CapacityBatteryMode, mode)
		site.publish(keys.CapacityBattery, site.capacityBatteryStatus())
	}

	return nil
}

// GetCapacityFastBattery returns how far the battery may support fast charging
func (site *Site) GetCapacityFastBattery() string {
	c := site.capacityCtl
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.bat.ctl.Settings().FastBattery)
}

// SetCapacityFastBattery sets how far the battery may support fast charging: buffer or reserve
func (site *Site) SetCapacityFastBattery(v string) error {
	fb := capacity.FastBattery(v)
	if !slices.Contains(capacity.FastBatteries, fb) {
		return fmt.Errorf("invalid capacity fast battery: %s", v)
	}

	c := site.capacityCtl
	if c == nil {
		return errors.New("capacity control not available")
	}

	c.mu.Lock()
	s := c.bat.ctl.Settings()
	changed := s.FastBattery != fb
	s.FastBattery = fb
	c.bat.ctl.SetSettings(s)
	c.mu.Unlock()

	if changed {
		settings.SetString(keys.CapacityFastBattery, v)
		site.publish(keys.CapacityFastBattery, v)
	}

	return nil
}

// GetCapacityReserveMode returns the reserve mode: dynamic or fixed
func (site *Site) GetCapacityReserveMode() string {
	c := site.capacityCtl
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.bat.reserve.Mode)
}

// SetCapacityReserveMode sets the reserve mode: dynamic or fixed
func (site *Site) SetCapacityReserveMode(v string) error {
	mode := capacity.ReserveMode(v)
	if !slices.Contains(capacity.ReserveModes, mode) {
		return fmt.Errorf("invalid capacity reserve mode: %s", v)
	}

	c := site.capacityCtl
	if c == nil {
		return errors.New("capacity control not available")
	}

	c.mu.Lock()
	changed := c.bat.reserve.Mode != mode
	c.bat.reserve.Mode = mode
	c.bat.planSlot = time.Time{}
	c.mu.Unlock()

	if changed {
		settings.SetString(keys.CapacityReserveMode, v)
		site.publish(keys.CapacityReserveMode, v)
	}

	return nil
}

// GetCapacityGridCharge returns whether the batteries charge from the grid on forecast
func (site *Site) GetCapacityGridCharge() bool {
	c := site.capacityCtl
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bat.gridCharge
}

// SetCapacityGridCharge sets whether the batteries charge from the grid on forecast
func (site *Site) SetCapacityGridCharge(enable bool) error {
	c := site.capacityCtl
	if c == nil {
		return errors.New("capacity control not available")
	}

	c.mu.Lock()
	changed := c.bat.gridCharge != enable
	c.bat.gridCharge = enable
	c.mu.Unlock()

	if changed {
		settings.SetBool(keys.CapacityGridCharge, enable)
		site.publish(keys.CapacityGridCharge, enable)
	}

	return nil
}

// GetCapacityBatteryPowers returns the configured charge and discharge power per battery in W
func (site *Site) GetCapacityBatteryPowers() map[string]float64 {
	c := site.capacityCtl
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.bat.powers)
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

	c := site.capacityCtl
	if c == nil {
		return errors.New("capacity control not available")
	}

	c.mu.Lock()
	changed := c.bat.powers[name] != power
	if power == 0 {
		delete(c.bat.powers, name)
	} else {
		c.bat.powers[name] = power
	}
	delete(c.bat.assumed, name)
	c.bat.planSlot = time.Time{}
	powers := maps.Clone(c.bat.powers)
	c.mu.Unlock()

	if changed {
		if err := settings.SetJson(keys.CapacityBatteryPowers, powers); err != nil {
			return err
		}
		site.publish(keys.CapacityBatteryPowers, powers)
	}

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
	c := site.capacityCtl
	if c == nil {
		return CapacityOff
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return effectiveBatteryMode(c.mode, c.bat.mode)
}

// capacityBatteryUnits returns the controllable batteries as measured this cycle. A battery
// without soc or capacity counts as not reachable.
func (site *Site) capacityBatteryUnits() capacity.BatteryUnits {
	devices := site.state().battery.Devices

	c := site.capacityCtl
	c.mu.Lock()
	powers := maps.Clone(c.bat.powers)
	c.mu.Unlock()

	var res capacity.BatteryUnits
	for i, dev := range site.batteryMeters {
		if !api.HasCap[api.BatteryController](dev.Instance()) {
			continue
		}

		u := capacity.BatteryUnit{Name: dev.Config().Name, MaxSoc: 100}
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

		u.MaxPower = powers[u.Name]
		if u.MaxPower == 0 {
			u.MaxPower = u.Capacity * capacityBatteryPowerPerKWh
			c.mu.Lock()
			if u.Capacity > 0 && !c.bat.assumed[u.Name] {
				c.bat.assumed[u.Name] = true
				site.log.INFO.Printf("capacity: battery %s assumed at %.0fW (%.1f kWh), set its power for exact rules", deviceTitleOrName(dev), u.MaxPower, u.Capacity)
			}
			c.mu.Unlock()
		}

		res = append(res, u)
	}

	return res
}

// capacityVehicleState returns whether a vehicle charges, and whether fast
func (site *Site) capacityVehicleState() capacity.VehicleState {
	var res capacity.VehicleState
	for _, lp := range site.activeLoadpoints() {
		if lp.charging() {
			res.Charging = true
			if lp.IsFastChargingActive() {
				res.Fast = true
			}
		}
	}
	return res
}

// capacityHouseProfiles returns the house profile at the 90th and 50th percentile in W per
// slot for n slots from now, and the days of history behind it
func (site *Site) capacityHouseProfiles(n int) (p90, p50 []float64, days int, err error) {
	col, ok := site.collectors[metrics.Home]
	if !ok {
		return nil, nil, 0, errors.New("no home collector")
	}

	from := now.BeginningOfDay().AddDate(0, 0, -28)
	if days, err = col.ProfileDays(from); err != nil {
		return nil, nil, 0, err
	}

	toW := func(v float64, _ int) float64 { return capacity.SlotPower(v) }
	for _, pc := range []struct {
		p   float64
		res *[]float64
	}{{0.9, &p90}, {0.5, &p50}} {
		profile, err := col.EnergyProfilePercentile(from, pc.p)
		if err != nil {
			return nil, nil, days, err
		}
		*pc.res = lo.Map(tileAndTrim(profile[:], n), toW)
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

	c.mu.Lock()
	b := &c.bat
	fresh := b.planSlot.Equal(slot) && b.vehicle == vehicle
	plan, rs := b.plan, b.reserve
	target := capacity.Target(c.settings, c.monthPeak.Power)
	c.mu.Unlock()

	if fresh {
		return plan
	}

	// two days cover the deadline of tomorrow
	const n = 2 * 96
	p90, p50, days, err := site.capacityHouseProfiles(n)
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
	b.plan, b.planSlot, b.vehicle = plan, slot, vehicle
	c.mu.Unlock()

	if len(plan.Slots) > 0 {
		site.log.DEBUG.Printf("capacity: reserve %.2f kWh, stored %.2f kWh, solar %.2f kWh, grid charge %.2f kWh in %d slots from %s",
			plan.Reserve, plan.Energy, plan.Surplus, plan.Need, len(plan.Slots), plan.Slots[0].Start.In(capacity.Location).Format("15:04"))
	}

	return plan
}

// updateCapacityBattery decides the battery modes after the slot budget, before the loadpoints.
// Loadpoints then limit against grid power plus the shift the new modes cause.
func (site *Site) updateCapacityBattery(measured bool) {
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
	units := site.capacityBatteryUnits()
	vehicle := site.capacityVehicleState()
	plan := site.capacityReservePlan(at, units, vehicle)

	c.mu.Lock()
	b := &c.bat
	in := capacity.BatteryInput{
		Now:       at,
		Units:     units,
		Measured:  measured && c.valid && !c.fallback,
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
	if mode == CapacityOn {
		c.gridPower += d.GridShift
		wd := b.ctl.Watchdog(in)
		b.resend = append(b.resend, wd.Resend...)
		for _, name := range wd.Resend {
			site.log.WARN.Printf("capacity: battery %s does not follow its mode, sending it again", name)
		}
		for _, name := range wd.Failed {
			site.log.ERROR.Printf("capacity: battery %s did not follow its mode after %d attempts, normal until restart", name, b.ctl.Settings().WatchdogRetries)
		}
	}
	b.decision, b.units = &d, units

	var changes []capacity.UnitDecision
	for _, u := range d.Units {
		if prev, ok := b.logged[u.Name]; !ok || prev.Mode != u.Mode || prev.Rule != u.Rule && u.Rule != capacity.RuleCalm {
			if u.Rule != capacity.RuleUnreachable {
				b.logged[u.Name] = u
				changes = append(changes, u)
			}
		}
	}
	c.mu.Unlock()

	verb := "battery"
	if mode != CapacityOn {
		verb = "would set battery"
	}
	for _, u := range changes {
		site.log.INFO.Printf("capacity: %s %s to %s (%s)", verb, u.Name, u.Mode, u.Rule)
	}

	site.publish(keys.CapacityBattery, site.capacityBatteryStatus())
}

// capacityBatteryAvailable returns the power the home battery can deliver for an appliance.
// With battery control on it is the sum of the units in normal above their minimum, else the
// site estimate of F1.
func (site *Site) capacityBatteryAvailable() float64 {
	if site.capacityBatteryEffective() == CapacityOn {
		c := site.capacityCtl
		c.mu.Lock()
		defer c.mu.Unlock()
		if d := c.bat.decision; d != nil {
			return d.Available
		}
		return 0
	}

	return site.capacityBatteryAvailableSite()
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

	if site.GetBatteryModeExternal() != api.BatteryUnknown {
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
	pending, d := c.batteryStart, c.bat.decision
	resend := c.bat.resend
	c.bat.resend = nil
	c.mu.Unlock()

	// the batteries get normal at start first, nothing else sends a mode meanwhile
	if pending || d == nil {
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
		if u.Rule == capacity.RuleUnreachable {
			continue
		}

		dev, ok := site.capacityBatteryDevice(u.Name)
		if !ok {
			continue
		}
		ctrl, ok := api.Cap[api.BatteryController](dev.Instance())
		if !ok {
			continue
		}

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
	res.Energy, res.Capacity = b.units.Energy(), b.units.Capacity()
	res.Reserve = b.plan.Reserve
	res.Available = b.decision.Available
	plan := b.plan
	res.Plan = &plan
	if b.ctl.Settings().FastBattery == capacity.FastBatteryBuffer {
		res.FastFloor = math.Min(b.plan.Reserve, math.Max(b.ctl.Settings().Buffer, b.units.MinEnergy()))
	} else {
		res.FastFloor = b.plan.Reserve
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
