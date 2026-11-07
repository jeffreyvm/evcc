package core

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/capacity"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/core/metrics"
	"github.com/evcc-io/evcc/core/site"
	"github.com/evcc-io/evcc/db/settings"
)

// capacity tariff modes
const (
	CapacityOff    = "off"
	CapacityShadow = "shadow" // decide and publish, never limit
	CapacityOn     = "on"
)

// CapacityModes are the valid capacity tariff modes
var CapacityModes = []string{CapacityOff, CapacityShadow, CapacityOn}

// capacityBatterySoc is the site battery soc above which the battery counts as available
// to cover an appliance in the cooking window. Per-unit availability follows with battery control.
const capacityBatterySoc = 20.0

// capacityLogStep is the ceiling change in W, about 1 A single phase, that is logged again
const capacityLogStep = 230.0

var _ site.CapacityAPI = (*Site)(nil)

// capacityStatus is published as keys.Capacity
type capacityStatus struct {
	Mode            string     `json:"mode"`
	Updated         *time.Time `json:"updated"`         // last control cycle, a heartbeat for Home Assistant, null before the first
	Target          float64    `json:"target"`          // W
	MonthPeak       float64    `json:"monthPeak"`       // W
	MonthPeakTime   *time.Time `json:"monthPeakTime"`   // start of the peak slot, null if none
	MonthPeakSource string     `json:"monthPeakSource"` // meter or evcc
	MonthPeakEvcc   float64    `json:"monthPeakEvcc"`   // W, own count as a check on the meter
	SlotStart       *time.Time `json:"slotStart"`       // current slot, null without a budget
	SlotEnergy      float64    `json:"slotEnergy"`      // kWh including assumed energy
	SlotAssumed     float64    `json:"slotAssumed"`     // kWh assumed after a restart mid-slot
	SlotAverage     float64    `json:"slotAverage"`     // W average so far
	Projection      float64    `json:"projection"`      // W projected slot average
	Budget          float64    `json:"budget"`          // W allowed for the rest of the slot
	Exceeded        bool       `json:"exceeded"`        // projection above target
	Threat          bool       `json:"threat"`          // peak threat
	MeterFallback   bool       `json:"meterFallback"`   // grid measurement lost, loadpoints at minimum
	SlowMeters      bool       `json:"slowMeters"`      // reading the meters took too long, loadpoints at minimum

	LastSlot *capacity.SlotResult `json:"lastSlot"` // last completed slot and whether charging exceeded it, null if none
}

// siteCapacity holds the capacity tariff state of a site
type siteCapacity struct {
	mu    sync.Mutex
	clock clock.Clock // mockable time

	mode     string
	settings capacity.Settings
	watch    *capacity.PeakWatch
	limiters map[*Loadpoint]*capacity.Limiter
	logged   map[*Loadpoint]capacity.Decision // last logged decision per loadpoint

	valid     bool // budget is based on a measurement
	budget    capacity.Budget
	gridPower float64
	// limitPower is what the loadpoints limit against: grid power plus what the batteries discharge.
	// Battery power covering a house peak is no headroom for the vehicle, or the reserve ends up in it.
	limitPower float64
	charging   int       // loadpoints charging at the last measurement, sharing the free budget
	measured   time.Time // last successful grid measurement
	fallback   bool
	slow       bool // last meter read took longer than SlowMeters
	threat     bool

	updated  time.Time // last control cycle
	slots    capacity.SlotTracker
	lastSlot *capacity.SlotResult

	peaks       capacity.MonthPeakTracker
	monthPeak   capacity.MonthPeakState
	peakChecked time.Time // slot of the last own count query
	peakErr     string    // last meter month peak error logged

	batteryStart bool   // batteries still to be put in normal mode after the start
	batteryErr   string // last error logged while doing so

	bat capacityBattery // battery control
}

func newSiteCapacity() *siteCapacity {
	s := capacity.DefaultSettings()
	c := &siteCapacity{
		clock:    clock.New(),
		mode:     CapacityOff,
		settings: s,
		watch:    capacity.NewPeakWatch(s.ThreatDelay),
		bat:      newCapacityBattery(),
	}
	c.reset()
	return c
}

// reset starts afresh: no hysteresis, threat or budget from an earlier mode
func (c *siteCapacity) reset() {
	c.limiters = make(map[*Loadpoint]*capacity.Limiter)
	c.logged = make(map[*Loadpoint]capacity.Decision)
	c.watch.Reset()
	c.slots.Reset()
	c.lastSlot = nil
	c.valid, c.fallback, c.slow = false, false, false
	c.measured = c.clock.Now() // a grid meter down from the start still times out
	c.bat.resetControl()
}

// limiter returns the limiter of a loadpoint, created on first use
func (c *siteCapacity) limiter(lp *Loadpoint) *capacity.Limiter {
	l, ok := c.limiters[lp]
	if !ok {
		l = capacity.NewLimiter(c.settings)
		c.limiters[lp] = l
	}
	return l
}

// restoreCapacitySettings restores the persisted capacity settings
func (site *Site) restoreCapacitySettings() {
	if site.capacityCtl == nil {
		return
	}
	if v, err := settings.Float(keys.CapacityMargin); err == nil {
		if err := site.SetCapacityMargin(v); err != nil {
			site.log.WARN.Printf("capacity margin: %v", err)
		}
	}
	if v, err := settings.Float(keys.CapacityFloor); err == nil {
		if err := site.SetCapacityFloor(v); err != nil {
			site.log.WARN.Printf("capacity floor: %v", err)
		}
	}
	if v, err := settings.Bool(keys.CapacityCooking); err == nil {
		if err := site.SetCapacityCooking(v); err != nil {
			site.log.WARN.Printf("capacity cooking: %v", err)
		}
	}
	if v, err := settings.String(keys.CapacityCookingWeekday); err == nil {
		if err := site.SetCapacityCookingWeekday(v); err != nil {
			site.log.WARN.Printf("capacity cooking weekday: %v", err)
		}
	}
	if v, err := settings.String(keys.CapacityCookingWeekend); err == nil {
		if err := site.SetCapacityCookingWeekend(v); err != nil {
			site.log.WARN.Printf("capacity cooking weekend: %v", err)
		}
	}
	if v, err := settings.String(keys.CapacityMode); err == nil && v != "" {
		if err := site.SetCapacityMode(v); err != nil {
			site.log.WARN.Printf("capacity mode: %v", err)
		}
	}
	site.publish(keys.CapacityMode, site.GetCapacityMode())
	site.publish(keys.CapacityFloor, site.GetCapacityFloor())
	site.publish(keys.CapacityMargin, site.GetCapacityMargin())
	site.publish(keys.CapacityCooking, site.GetCapacityCooking())
	site.publish(keys.CapacityCookingWeekday, site.GetCapacityCookingWeekday())
	site.publish(keys.CapacityCookingWeekend, site.GetCapacityCookingWeekend())

	site.restoreCapacityBatterySettings()
}

// GetCapacityMode returns the capacity tariff mode
func (site *Site) GetCapacityMode() string {
	c := site.capacityCtl
	if c == nil {
		return CapacityOff
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mode
}

// capacityImportLimit returns the enforced capacity target in W, zero if capacity is not on
func (site *Site) capacityImportLimit() float64 {
	c := site.capacityCtl
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mode != CapacityOn {
		return 0
	}
	return capacity.Target(c.settings, c.monthPeak.Power)
}

// SetCapacityMode sets the capacity tariff mode: off, shadow or on
func (site *Site) SetCapacityMode(mode string) error {
	if !slices.Contains(CapacityModes, mode) {
		return fmt.Errorf("invalid capacity mode: %s", mode)
	}
	// a configured grid meter that failed to start still allows on: the loadpoints then
	// charge at minimum current until it measures, instead of unlimited
	if mode != CapacityOff && site.Meters.GridMeterRef == "" {
		return fmt.Errorf("capacity mode %s requires a grid meter", mode)
	}

	c := site.capacityCtl
	if c == nil {
		return fmt.Errorf("capacity control not available")
	}
	c.mu.Lock()
	changed := c.mode != mode
	c.mode = mode
	if changed {
		c.reset()
	}
	c.mu.Unlock()

	if changed {
		site.log.INFO.Printf("capacity mode: %s", mode)
		settings.SetString(keys.CapacityMode, mode)
		// the status is not updated while off, publish the reset state
		site.publish(keys.Capacity, site.capacityStatus())
		site.publish(keys.CapacityBattery, site.capacityBatteryStatus())
		site.publish(keys.CapacityMode, mode)
	}

	return nil
}

// GetCapacityFloor returns the target peak floor in W
func (site *Site) GetCapacityFloor() float64 {
	c := site.capacityCtl
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.settings.Floor
}

// SetCapacityFloor sets the target peak floor in W. The target is never raised automatically.
func (site *Site) SetCapacityFloor(floor float64) error {
	if math.IsNaN(floor) || floor < capacity.MinFloor || floor > capacity.MaxFloor {
		return fmt.Errorf("invalid capacity floor: %v, must be %.0f to %.0fW", floor, capacity.MinFloor, capacity.MaxFloor)
	}

	if site.updateCapacitySetting(func(s *capacity.Settings) *float64 { return &s.Floor }, floor) {
		settings.SetFloat(keys.CapacityFloor, floor)
		site.publish(keys.CapacityFloor, floor)
	}

	return nil
}

// GetCapacityMargin returns the control margin in W
func (site *Site) GetCapacityMargin() float64 {
	c := site.capacityCtl
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.settings.Margin
}

// SetCapacityMargin sets the control margin in W. Its bound lies below the lowest floor,
// so floor and margin never need to be checked against each other.
func (site *Site) SetCapacityMargin(margin float64) error {
	if math.IsNaN(margin) || margin < 0 || margin > capacity.MaxMargin {
		return fmt.Errorf("invalid capacity margin: %v, must be 0 to %.0fW", margin, capacity.MaxMargin)
	}

	if site.updateCapacitySetting(func(s *capacity.Settings) *float64 { return &s.Margin }, margin) {
		settings.SetFloat(keys.CapacityMargin, margin)
		site.publish(keys.CapacityMargin, margin)
	}

	return nil
}

// GetCapacityCooking returns whether the cooking window is on
func (site *Site) GetCapacityCooking() bool {
	return capacityCookingGet(site, false, func(ws capacity.Windows) bool { return !ws.Off })
}

// SetCapacityCooking switches the cooking window on or off. Off keeps the configured windows.
func (site *Site) SetCapacityCooking(enable bool) error {
	changed, err := site.updateCapacityCooking(func(ws *capacity.Windows) bool {
		if ws.Off == !enable {
			return false
		}
		ws.Off = !enable
		return true
	})
	if err != nil || !changed {
		return err
	}

	site.log.INFO.Printf("capacity cooking window: %t", enable)
	settings.SetBool(keys.CapacityCooking, enable)
	site.publish(keys.CapacityCooking, enable)
	return nil
}

// GetCapacityCookingWeekday returns the cooking windows on weekdays as HH:MM-HH:MM, comma separated
func (site *Site) GetCapacityCookingWeekday() string {
	return capacityCookingGet(site, "", func(ws capacity.Windows) string { return capacity.FormatWindows(ws.Weekday) })
}

// SetCapacityCookingWeekday sets the cooking windows on weekdays, empty or none for no window
func (site *Site) SetCapacityCookingWeekday(v string) error {
	return site.setCapacityCookingWindows(v, keys.CapacityCookingWeekday, func(ws *capacity.Windows) *[]capacity.Window { return &ws.Weekday })
}

// GetCapacityCookingWeekend returns the cooking windows on weekends as HH:MM-HH:MM, comma separated
func (site *Site) GetCapacityCookingWeekend() string {
	return capacityCookingGet(site, "", func(ws capacity.Windows) string { return capacity.FormatWindows(ws.Weekend) })
}

// SetCapacityCookingWeekend sets the cooking windows on weekends, empty or none for no window
func (site *Site) SetCapacityCookingWeekend(v string) error {
	return site.setCapacityCookingWindows(v, keys.CapacityCookingWeekend, func(ws *capacity.Windows) *[]capacity.Window { return &ws.Weekend })
}

func (site *Site) setCapacityCookingWindows(v, key string, field func(*capacity.Windows) *[]capacity.Window) error {
	res, err := capacity.ParseWindows(v)
	if err != nil {
		return err
	}

	changed, err := site.updateCapacityCooking(func(ws *capacity.Windows) bool {
		p := field(ws)
		if slices.Equal(*p, res) {
			return false
		}
		*p = res
		return true
	})
	if err != nil || !changed {
		return err
	}

	formatted := capacity.FormatWindows(res)
	site.log.INFO.Printf("capacity cooking window %s: %s", strings.TrimPrefix(key, keys.CapacityCooking), formatted)
	settings.SetString(key, formatted)
	site.publish(key, formatted)
	return nil
}

// capacityCookingGet reads the cooking windows under the capacity lock, def without capacity control
func capacityCookingGet[T any](site *Site, def T, get func(capacity.Windows) T) T {
	c := site.capacityCtl
	if c == nil {
		return def
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return get(c.settings.Cooking)
}

// updateCapacityCooking changes the cooking windows and hands them to the limiters if set changed them
func (site *Site) updateCapacityCooking(set func(*capacity.Windows) bool) (bool, error) {
	c := site.capacityCtl
	if c == nil {
		return false, errors.New("capacity control not available")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if !set(&c.settings.Cooking) {
		return false, nil
	}
	for _, l := range c.limiters {
		l.SetSettings(c.settings)
	}
	return true, nil
}

// updateCapacitySetting sets the setting field points to and reports whether it changed
func (site *Site) updateCapacitySetting(field func(*capacity.Settings) *float64, v float64) bool {
	c := site.capacityCtl
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	p := field(&c.settings)
	if *p == v {
		return false
	}
	*p = v

	for _, l := range c.limiters {
		l.SetSettings(c.settings)
	}
	return true
}

// updateCapacity updates the slot budget after the meters have been read, which took the
// given time. Without a grid measurement for MeterTimeout, charging loadpoints fall back to
// minimum current. A read slower than SlowMeters, e.g. a hanging battery, limits to minimum
// current for that cycle, since the grid power is stale by then.
func (site *Site) updateCapacity(measured bool, took time.Duration) {
	c := site.capacityCtl
	mode := site.GetCapacityMode()

	if c == nil || mode == CapacityOff {
		return
	}

	// a grid meter that failed to start never measures
	if site.gridMeter == nil {
		measured = false
	}

	// battery modes are decided after the budget, before the loadpoints limit against it
	defer site.updateCapacityBattery()

	now := c.clock.Now()

	c.mu.Lock()
	c.updated = now
	c.mu.Unlock()

	if !measured {
		c.mu.Lock()
		c.watch.Reset()
		fallback := now.Sub(c.measured) >= c.settings.MeterTimeout
		if fallback && !c.fallback {
			verb := "limiting"
			if mode != CapacityOn {
				verb = "would limit"
			}
			site.log.WARN.Printf("capacity: grid measurement lost, %s loadpoints to minimum current", verb)
		}
		c.fallback, c.valid = fallback, !fallback && c.valid
		c.mu.Unlock()

		if fallback && mode == CapacityOn {
			for _, lp := range site.activeLoadpoints() {
				lp.capacityFallback()
			}
		}

		site.publish(keys.Capacity, site.capacityStatus())
		return
	}

	col, ok := site.collectors[site.gridMeter.Config().Name]
	if !ok {
		return
	}

	site.refreshMonthPeak(col, now)

	energy, from := col.SlotEnergy()
	state := site.state()
	gridPower := state.gridPower
	limitPower := gridPower
	if p := state.battery.Power; p > 0 && !math.IsNaN(p) && !math.IsInf(p, 0) {
		limitPower += p
	}

	var (
		chargePower float64
		charging    int
	)
	for _, lp := range site.activeLoadpoints() {
		chargePower += lp.GetChargePower()
		if lp.charging() {
			charging++
		}
	}

	c.mu.Lock()
	c.budget = capacity.Compute(c.settings, capacity.Measurement{
		Time:        now,
		GridPower:   gridPower,
		SlotEnergy:  energy,
		CountedFrom: from,
		MonthPeak:   c.monthPeak.Power,
	})
	c.threat = c.watch.Update(c.budget, gridPower, now)
	c.gridPower = gridPower
	c.limitPower = limitPower
	c.charging = charging
	c.measured = now
	c.valid = true
	if slow := took > c.settings.SlowMeters; slow != c.slow {
		c.slow = slow
		if slow {
			site.log.WARN.Printf("capacity: reading the meters took %s, loadpoints at most minimum current", took.Round(time.Second))
		} else {
			site.log.INFO.Println("capacity: meter reads fast again")
		}
	}
	if c.fallback {
		site.log.INFO.Println("capacity: grid measurement restored")
	}
	c.fallback = false
	b := c.budget
	slot, completed := c.slots.Update(b, chargePower, now)
	if completed {
		c.lastSlot = &slot
	}
	c.mu.Unlock()

	if completed {
		site.logCapacitySlot(slot)
	}

	site.log.DEBUG.Printf("capacity: target %.0fW, slot %.3fkWh, budget %.0fW, projection %.0fW", b.Target, b.Energy, b.Rest, b.ProjectedPower())

	site.publish(keys.Capacity, site.capacityStatus())
}

// logCapacitySlot logs a completed slot, as a warning when it exceeded the target
func (site *Site) logCapacitySlot(r capacity.SlotResult) {
	cause := "by the house"
	if r.ByVehicle {
		cause = "by charging"
	}
	partial := ""
	if r.Partial {
		partial = ", estimated"
	}

	msg := fmt.Sprintf("capacity: slot %s averaged %.0fW (house %.0fW, charging %.0fW), target %.0fW%s",
		r.Start.In(capacity.Location).Format("02.01. 15:04"), r.Average, r.House(), r.Vehicle, r.Target, partial)

	if r.Exceeded {
		site.log.WARN.Printf("%s: exceeded %s", msg, cause)
	} else {
		site.log.DEBUG.Println(msg)
	}
}

// refreshMonthPeak updates the month peak. The meter is read every cycle, since it registers
// a slot up to a minute after its end. The own count is queried once per slot.
func (site *Site) refreshMonthPeak(col *metrics.Collector, now time.Time) {
	c := site.capacityCtl
	slot := capacity.SlotStart(now)

	c.mu.Lock()
	checked := c.peakChecked.Equal(slot)
	c.mu.Unlock()

	if !checked {
		site.refreshEvccMonthPeak(col, now, slot)
	}

	meter, meterErr := site.meterMonthPeak(now)

	c.mu.Lock()
	defer c.mu.Unlock()

	switch {
	case meterErr == nil:
		c.peaks.SetMeter(meter, now)
		c.peakErr = ""
	case !errors.Is(meterErr, api.ErrNotAvailable) && meterErr.Error() != c.peakErr:
		c.peakErr = meterErr.Error()
		site.log.WARN.Printf("capacity: meter month peak: %v", meterErr)
	}

	st := c.peaks.Update(now, capacity.Location)
	c.monthPeak = st

	if st.Switched {
		site.log.INFO.Printf("capacity: month peak from %s: %.0fW", st.Source, st.Power)
	}
	if st.Mismatch {
		site.log.WARN.Printf("capacity: month peak of the meter %.0fW differs from own count %.0fW", st.Power, st.Evcc.Power)
	}
}

// refreshEvccMonthPeak reads the month peak from the persisted grid slots
func (site *Site) refreshEvccMonthPeak(col *metrics.Collector, now, slot time.Time) {
	energy, at, err := col.MaxSlotEnergySince(capacity.MonthStart(now, capacity.Location))
	if err != nil {
		site.log.ERROR.Printf("capacity: month peak: %v", err)
		return
	}

	c := site.capacityCtl
	c.mu.Lock()
	defer c.mu.Unlock()

	c.peakChecked = slot
	c.peaks.SetEvcc(capacity.Peak{Power: capacity.SlotPower(energy), Start: at})
}

// meterMonthPeak reads the month peak registered by the grid meter
func (site *Site) meterMonthPeak(now time.Time) (capacity.Peak, error) {
	m, ok := api.Cap[api.MeterMonthPeak](site.gridMeter.Instance())
	if !ok {
		return capacity.Peak{}, api.ErrNotAvailable
	}

	power, registered, err := m.MonthPeak()
	if err != nil {
		return capacity.Peak{}, err
	}

	return capacity.MonthPeakFromMeter(power, registered, now, capacity.Location)
}

// timeOrNil returns nil for the zero time. The status always carries every field: the UI
// merges published objects, so an omitted field would keep its previous value.
func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (site *Site) capacityStatus() capacityStatus {
	c := site.capacityCtl
	c.mu.Lock()
	defer c.mu.Unlock()

	res := capacityStatus{
		Mode:            c.mode,
		Updated:         timeOrNil(c.updated),
		MonthPeak:       c.monthPeak.Power,
		MonthPeakTime:   timeOrNil(c.monthPeak.Start),
		MonthPeakSource: c.monthPeak.Source,
		MonthPeakEvcc:   c.monthPeak.Evcc.Power,
		Target:          capacity.Target(c.settings, c.monthPeak.Power),
		MeterFallback:   c.fallback,
		SlowMeters:      c.slow,
		LastSlot:        c.lastSlot,
	}

	if c.valid {
		b := c.budget
		res.SlotStart = timeOrNil(b.SlotStart)
		res.SlotEnergy = b.Energy
		res.SlotAssumed = b.Assumed
		res.SlotAverage = b.AveragePower()
		res.Projection = b.ProjectedPower()
		res.Budget = b.Rest
		res.Exceeded = b.Exceeded()
		res.Threat = c.threat
	}

	return res
}

// capacityBatteryStart marks every battery to be put in normal mode when evcc starts with the
// capacity mode on or in shadow and battery control on. After a crash a battery can still be held
// or charging from the grid, and evcc does not know: the battery mode is not persisted and is only
// sent on change. Without battery control evcc sent no such mode, and normal could undo a mode the
// battery runs on its own, such as the peak shaving of the Indevolt.
func (site *Site) capacityBatteryStart() {
	c := site.capacityCtl
	if c == nil || site.GetCapacityMode() == CapacityOff || site.GetCapacityBatteryMode() != CapacityOn || len(site.batteryMeters) == 0 {
		return
	}

	c.mu.Lock()
	c.batteryStart = true
	c.mu.Unlock()
}

// capacityBatteryNormal puts the batteries in normal mode after the start, once the meters have
// been read. A battery that is not reachable yet is tried again every cycle, until each one
// accepted normal or the regular battery control set a mode of its own.
func (site *Site) capacityBatteryNormal() {
	c := site.capacityCtl
	if c == nil {
		return
	}

	c.mu.Lock()
	pending := c.batteryStart
	c.mu.Unlock()

	if !pending {
		return
	}

	// battery control sends each unit its mode itself, normal or stricter, and retries a unit
	// not reachable: one hanging unit must not keep the other from being controlled
	if site.capacityBatteryEffective() == CapacityOn {
		c.mu.Lock()
		c.batteryStart = false
		c.mu.Unlock()
		return
	}

	if site.GetBatteryMode() != api.BatteryUnknown {
		c.mu.Lock()
		c.batteryStart = false
		c.mu.Unlock()
		return
	}

	if site.batteryModeApplied == nil {
		site.batteryModeApplied = make(map[string]api.BatteryMode)
	}

	var errs []error
	for _, dev := range site.batteryMeters {
		batCtrl, ok := api.Cap[api.BatteryController](dev.Instance())
		if !ok {
			continue
		}

		name := dev.Config().Name
		mode := supportedBatteryMode(batCtrl.BatteryModes(), api.BatteryNormal)
		if mode == api.BatteryUnknown || site.batteryModeApplied[name] == mode {
			continue
		}

		if err := batCtrl.SetBatteryMode(mode); err != nil {
			if !errors.Is(err, api.ErrNotAvailable) {
				errs = append(errs, fmt.Errorf("%s: %w", deviceTitleOrName(dev), err))
			}
			continue
		}

		site.batteryModeApplied[name] = mode
	}

	c.mu.Lock()
	if err := errors.Join(errs...); err != nil {
		if msg := err.Error(); msg != c.batteryErr {
			c.batteryErr = msg
			site.log.WARN.Printf("capacity: batteries to normal at start, retrying: %v", err)
		}
		c.mu.Unlock()
		return
	}
	c.batteryStart, c.batteryErr = false, ""
	c.mu.Unlock()

	// outside the lock: the site lock is never taken while holding the capacity lock
	site.SetBatteryMode(api.BatteryNormal)
	site.log.INFO.Println("capacity: batteries set to normal at start")
}

// capacityBatteryAvailableSite estimates the power the home battery can deliver for an appliance
// without battery control. A held or grid charging battery delivers nothing, otherwise the charge
// decides (see capacityBatterySoc).
func (site *Site) capacityBatteryAvailableSite() float64 {
	if len(site.batteryMeters) == 0 {
		return 0
	}

	for _, mode := range []api.BatteryMode{site.GetBatteryMode(), site.GetBatteryModeExternal()} {
		if mode == api.BatteryHold || mode == api.BatteryCharge {
			return 0
		}
	}

	state := site.state()
	if state.battery.Soc <= capacityBatterySoc {
		return 0
	}

	c := site.capacityCtl
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.settings.CookingBattery
}

// shared returns the budget for one loadpoint: the free part of the slot, Rest minus grid power,
// is split evenly over the charging loadpoints, so together they claim it only once. A loadpoint
// that is not charging yet counts as one more.
func (c *siteCapacity) shared(st capacity.LoadpointState) capacity.Budget {
	b := c.budget

	n := c.charging
	if st.ChargePower <= 0 {
		n++
	}
	if n > 1 {
		b.Rest = c.limitPower + (b.Rest-c.limitPower)/float64(n)
	}

	return b
}

// capacityIdle forgets the limiter state of a loadpoint that does not want to charge, so a
// pause from an earlier session or mode does not hold back the next one
func (site *Site) capacityIdle(lp *Loadpoint) {
	c := site.capacityCtl
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.limiters, lp)
	delete(c.logged, lp)
}

// capacityLimit returns the capacity decision for a loadpoint and whether it is enforced
func (site *Site) capacityLimit(lp *Loadpoint, st capacity.LoadpointState) (capacity.Decision, bool) {
	c := site.capacityCtl
	if c == nil {
		return capacity.Decision{MaxPower: st.MaxPower}, false
	}

	// before locking: the title getter takes the loadpoint lock
	title := lp.GetTitle()

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.mode == CapacityOff {
		return capacity.Decision{MaxPower: st.MaxPower}, false
	}

	var d capacity.Decision
	switch l := c.limiter(lp); {
	case !c.valid || c.fallback:
		// no budget, e.g. right after a mode change before the next measurement:
		// minimum current, never an unlimited fast charge
		d = l.Fallback(st)
	case c.slow:
		d = l.Stale(c.shared(st), c.limitPower, st)
	default:
		d = l.Limit(c.shared(st), c.limitPower, st)
	}

	enforce := c.mode == CapacityOn

	if prev := c.logged[lp]; prev.Limited != d.Limited || prev.Reason != d.Reason || math.Abs(prev.MaxPower-d.MaxPower) >= capacityLogStep {
		c.logged[lp] = d
		verb := "limit"
		if !enforce {
			verb = "would limit"
		}
		if d.Limited {
			site.log.INFO.Printf("capacity: %s %s to %.0fW (%s)", verb, title, d.MaxPower, d.Reason)
		} else if prev.Limited {
			site.log.INFO.Printf("capacity: %s no longer limited", title)
		}
	}

	return d, enforce
}
