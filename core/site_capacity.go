package core

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

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
	Mode          string    `json:"mode"`
	Target        float64   `json:"target"`                  // W
	MonthPeak     float64   `json:"monthPeak"`               // W
	MonthPeakTime time.Time `json:"monthPeakTime,omitzero"`  // start of the peak slot
	MonthPeakSrc  string    `json:"monthPeakSource"`         // meter or evcc
	MonthPeakEvcc float64   `json:"monthPeakEvcc"`           // W, own count as a check on the meter
	SlotStart     time.Time `json:"slotStart,omitzero"`      // current slot
	SlotEnergy    float64   `json:"slotEnergy"`              // kWh including assumed energy
	SlotAssumed   float64   `json:"slotAssumed,omitempty"`   // kWh assumed after a restart mid-slot
	SlotAverage   float64   `json:"slotAverage"`             // W average so far
	Projection    float64   `json:"projection"`              // W projected slot average
	Budget        float64   `json:"budget"`                  // W allowed for the rest of the slot
	Exceeded      bool      `json:"exceeded"`                // projection above target
	Threat        bool      `json:"threat"`                  // peak threat, used for battery release
	MeterFallback bool      `json:"meterFallback,omitempty"` // grid measurement lost, loadpoints at minimum
}

// siteCapacity holds the capacity tariff state of a site
type siteCapacity struct {
	mu sync.Mutex

	mode     string
	settings capacity.Settings
	watch    *capacity.PeakWatch
	limiters map[*Loadpoint]*capacity.Limiter
	logged   map[*Loadpoint]capacity.Decision // last logged decision per loadpoint

	valid     bool // budget is based on a measurement
	budget    capacity.Budget
	gridPower float64
	measured  time.Time // last successful grid measurement
	fallback  bool
	threat    bool

	peaks       capacity.MonthPeakTracker
	monthPeak   capacity.MonthPeakState
	peakChecked time.Time // slot of the last own count query
	peakErr     string    // last meter month peak error logged
}

func newSiteCapacity() *siteCapacity {
	s := capacity.DefaultSettings()
	c := &siteCapacity{
		mode:     CapacityOff,
		settings: s,
		watch:    capacity.NewPeakWatch(s.ThreatDelay),
	}
	c.reset()
	return c
}

// reset starts afresh: no hysteresis, threat or budget from an earlier mode
func (c *siteCapacity) reset() {
	c.limiters = make(map[*Loadpoint]*capacity.Limiter)
	c.logged = make(map[*Loadpoint]capacity.Decision)
	c.watch.Reset()
	c.valid, c.fallback = false, false
	c.measured = time.Now() // a grid meter down from the start still times out
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
	// margin first: it is checked against the default floor, which any valid margin is below
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
	if v, err := settings.String(keys.CapacityMode); err == nil && v != "" {
		if err := site.SetCapacityMode(v); err != nil {
			site.log.WARN.Printf("capacity mode: %v", err)
		}
	}
	site.publish(keys.CapacityMode, site.GetCapacityMode())
	site.publish(keys.CapacityFloor, site.GetCapacityFloor())
	site.publish(keys.CapacityMargin, site.GetCapacityMargin())
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

// SetCapacityMode sets the capacity tariff mode: off, shadow or on
func (site *Site) SetCapacityMode(mode string) error {
	if !slices.Contains(CapacityModes, mode) {
		return fmt.Errorf("invalid capacity mode: %s", mode)
	}
	if mode != CapacityOff && site.gridMeter == nil {
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
	}
	site.publish(keys.CapacityMode, mode)

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
	if math.IsNaN(floor) || math.IsInf(floor, 0) || floor <= site.GetCapacityMargin() {
		return fmt.Errorf("invalid capacity floor: %v, must exceed the margin", floor)
	}

	site.updateCapacitySettings(func(s *capacity.Settings) { s.Floor = floor })
	settings.SetFloat(keys.CapacityFloor, floor)
	site.publish(keys.CapacityFloor, floor)

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

// SetCapacityMargin sets the control margin in W
func (site *Site) SetCapacityMargin(margin float64) error {
	if margin < 0 || margin > 1000 || math.IsNaN(margin) || margin >= site.GetCapacityFloor() {
		return fmt.Errorf("invalid capacity margin: %v, must be below the floor", margin)
	}

	site.updateCapacitySettings(func(s *capacity.Settings) { s.Margin = margin })
	settings.SetFloat(keys.CapacityMargin, margin)
	site.publish(keys.CapacityMargin, margin)

	return nil
}

func (site *Site) updateCapacitySettings(fun func(*capacity.Settings)) {
	c := site.capacityCtl
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	fun(&c.settings)
	for _, l := range c.limiters {
		l.SetSettings(c.settings)
	}
}

// updateCapacity updates the slot budget after the meters have been read. Without a grid
// measurement for MeterTimeout, charging loadpoints fall back to minimum current.
func (site *Site) updateCapacity(measured bool) {
	c := site.capacityCtl
	mode := site.GetCapacityMode()

	if c == nil || mode == CapacityOff || site.gridMeter == nil {
		return
	}

	now := time.Now()

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
	gridPower := site.state().gridPower

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
	c.measured = now
	c.valid = true
	if c.fallback {
		site.log.INFO.Println("capacity: grid measurement restored")
	}
	c.fallback = false
	b := c.budget
	c.mu.Unlock()

	site.log.DEBUG.Printf("capacity: target %.0fW, slot %.3fkWh, budget %.0fW, projection %.0fW", b.Target, b.Energy, b.Rest, b.ProjectedPower())

	site.publish(keys.Capacity, site.capacityStatus())
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

	st := c.peaks.Update(now, time.Local)
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
	energy, at, err := col.MaxSlotEnergySince(capacity.MonthStart(now, time.Local))
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

	return capacity.MonthPeakFromMeter(power, registered, now, time.Local)
}

func (site *Site) capacityStatus() capacityStatus {
	c := site.capacityCtl
	c.mu.Lock()
	defer c.mu.Unlock()

	res := capacityStatus{
		Mode:          c.mode,
		MonthPeak:     c.monthPeak.Power,
		MonthPeakTime: c.monthPeak.Start,
		MonthPeakSrc:  c.monthPeak.Source,
		MonthPeakEvcc: c.monthPeak.Evcc.Power,
		Target:        capacity.Target(c.settings, c.monthPeak.Power),
		MeterFallback: c.fallback,
	}

	if c.valid {
		b := c.budget
		res.SlotStart = b.SlotStart
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

// capacityBatteryAvailable estimates the power the home battery can deliver for an appliance
func (site *Site) capacityBatteryAvailable() float64 {
	if len(site.batteryMeters) == 0 {
		return 0
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

// capacityLimit returns the capacity decision for a loadpoint and whether it is enforced.
// Each loadpoint may claim the whole free budget, which is right for a single loadpoint only.
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
	if c.valid && !c.fallback {
		d = c.limiter(lp).Limit(c.budget, c.gridPower, st)
	} else {
		// no budget, e.g. right after a mode change before the next measurement:
		// minimum current, never an unlimited fast charge
		d = capacity.Fallback(st)
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
