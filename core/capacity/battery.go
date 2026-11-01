package capacity

import (
	"cmp"
	"math"
	"slices"
	"time"
)

// BatteryMode is the mode the capacity rules ask of one battery unit
type BatteryMode string

const (
	BatteryNormal BatteryMode = "normal" // self-consumption, the unit follows the grid meter to zero
	BatteryHold   BatteryMode = "hold"   // stand-by, keeps its charge
	BatteryCharge BatteryMode = "charge" // charges from the grid at its fixed power
)

// BatteryRule names the rule that decided the mode of a unit, in the order they are checked
type BatteryRule string

const (
	RulePeak        BatteryRule = "peak"             // slot threatens above target: release the reserve
	RuleInjection   BatteryRule = "injection"        // exporting: hold would send solar power away
	RuleGridCharge  BatteryRule = "gridCharge"       // planned grid charge slot with room in the budget
	RuleVehicle     BatteryRule = "vehicle"          // the vehicle charges: down to its floor
	RuleReserve     BatteryRule = "reserve"          // at or below the reserve: hold
	RuleNormal      BatteryRule = "normal"           // self-consumption
	RuleMeter       BatteryRule = "meterUnavailable" // no grid measurement: normal, no grid charging
	RuleWatchdog    BatteryRule = "watchdog"         // unit did not follow its mode three times: normal until restart
	RuleUnreachable BatteryRule = "unreachable"      // unit not read this cycle: no command
	RuleCalm        BatteryRule = "calm"             // mode kept, changed less than a minute ago
)

// FastBattery is how far the battery may support fast charging
type FastBattery string

const (
	FastBatteryBuffer  FastBattery = "buffer"  // down to the peak buffer
	FastBatteryReserve FastBattery = "reserve" // down to the reserve, as in smart mode
)

// FastBatteries are the valid fast battery settings
var FastBatteries = []FastBattery{FastBatteryBuffer, FastBatteryReserve}

// BatterySettings are the tunable parameters of the battery rules
type BatterySettings struct {
	FastBattery FastBattery
	Buffer      float64 // kWh total, incl. the minimum, fast charging may use the battery down to
	Hysteresis  float64 // % soc above the reserve before a held unit is released

	InjectionPower float64       // W, grid power below this counts as export (negative)
	InjectionDelay time.Duration // export must persist this long
	Calm           time.Duration // minimum time between mode changes of a unit, except to release a peak
	AvailableSoc   float64       // % soc above the minimum for a unit to count as available

	WatchdogDelay   time.Duration // a unit must misbehave this long before its mode is sent again
	WatchdogImport  float64       // W import above which a unit in normal must deliver
	WatchdogSoc     float64       // % soc above the minimum for a unit in normal to be expected to deliver
	WatchdogIdle    float64       // W below which a unit counts as delivering nothing
	WatchdogHold    float64       // W a held unit may still charge or discharge
	WatchdogRetries int           // resends before the unit falls back to normal until restart
}

// DefaultBatterySettings returns the default battery settings
func DefaultBatterySettings() BatterySettings {
	return BatterySettings{
		FastBattery:     FastBatteryBuffer,
		Buffer:          1.5,
		Hysteresis:      2,
		InjectionPower:  -100,
		InjectionDelay:  time.Minute,
		Calm:            time.Minute,
		AvailableSoc:    2,
		WatchdogDelay:   2 * time.Minute,
		WatchdogImport:  300,
		WatchdogSoc:     5,
		WatchdogIdle:    20,
		WatchdogHold:    200,
		WatchdogRetries: 3,
	}
}

// BatteryUnit is one controllable home battery as measured this cycle
type BatteryUnit struct {
	Name      string
	Reachable bool    // read without error this cycle
	Soc       float64 // %
	MinSoc    float64 // %, the unit does not discharge below
	MaxSoc    float64 // %, grid charging stops here, 0 or 100 for none
	Capacity  float64 // kWh
	Power     float64 // W, positive discharging
	MaxPower  float64 // W, fixed charge power and maximum discharge power
}

// Energy returns the stored energy in kWh
func (u BatteryUnit) Energy() float64 {
	return u.Soc / 100 * u.Capacity
}

// MinEnergy returns the energy below which the unit does not discharge in kWh
func (u BatteryUnit) MinEnergy() float64 {
	return u.MinSoc / 100 * u.Capacity
}

// full reports whether the unit reached the soc that ends grid charging
func (u BatteryUnit) full() bool {
	maxSoc := u.MaxSoc
	if maxSoc <= 0 || maxSoc > 100 {
		maxSoc = 100
	}
	return u.Soc >= maxSoc
}

// BatteryUnits are the units of a site
type BatteryUnits []BatteryUnit

// reachable returns the units read this cycle
func (uu BatteryUnits) reachable() BatteryUnits {
	var res BatteryUnits
	for _, u := range uu {
		if u.Reachable && u.Capacity > 0 {
			res = append(res, u)
		}
	}
	return res
}

// Capacity returns the total capacity of the reachable units in kWh
func (uu BatteryUnits) Capacity() float64 {
	var res float64
	for _, u := range uu.reachable() {
		res += u.Capacity
	}
	return res
}

// Energy returns the energy stored in the reachable units in kWh
func (uu BatteryUnits) Energy() float64 {
	var res float64
	for _, u := range uu.reachable() {
		res += u.Energy()
	}
	return res
}

// MinEnergy returns the energy the reachable units keep at their minimum in kWh
func (uu BatteryUnits) MinEnergy() float64 {
	var res float64
	for _, u := range uu.reachable() {
		res += u.MinEnergy()
	}
	return res
}

// share returns the part of a site wide energy that falls to unit u, by capacity, never
// below the unit minimum. A unit that is not reachable gets nothing.
func (uu BatteryUnits) share(u BatteryUnit, total float64) float64 {
	capacity := uu.Capacity()
	if capacity <= 0 || !u.Reachable {
		return 0
	}
	return math.Max(u.MinEnergy(), total*u.Capacity/capacity)
}

// VehicleState is what the battery rules need to know about charging vehicles
type VehicleState struct {
	Charging bool // a loadpoint charges
	Fast     bool // one of them in fast mode or on a plan
}

// BatteryInput is the state of one control cycle for the battery rules
type BatteryInput struct {
	Now       time.Time
	Units     BatteryUnits
	Measured  bool    // budget and grid power are based on a fresh measurement
	Budget    Budget  // slot budget, valid if Measured
	GridPower float64 // W, positive is import
	Threat    bool    // slot threatens above target (PeakWatch)
	Vehicle   VehicleState

	Reserve    float64         // kWh site reserve target incl. the unit minimums
	GridCharge map[string]bool // units planned to charge from the grid in the current slot
}

// house returns the consumption without the batteries in W: grid power plus what they deliver
func (in BatteryInput) house() float64 {
	res := in.GridPower
	for _, u := range in.Units.reachable() {
		res += u.Power
	}
	return res
}

// UnitDecision is the mode for one unit
type UnitDecision struct {
	Name    string      `json:"name"`
	Mode    BatteryMode `json:"mode"`
	Rule    BatteryRule `json:"rule"`
	Reserve float64     `json:"reserve"` // kWh share of the reserve target
	Floor   float64     `json:"floor"`   // kWh the unit may support a charging vehicle down to
	Changed bool        `json:"-"`       // mode differs from the last decision
}

// BatteryDecision is the result of the battery rules for one cycle
type BatteryDecision struct {
	Units []UnitDecision

	// Available is the power in W the units in normal can deliver above their minimum, for an
	// appliance switching on. A unit at its minimum or held delivers nothing.
	Available float64

	// GridShift is the change of grid power in W the new modes cause: a discharging unit that is
	// held, or one that starts charging. Loadpoints limit against grid power plus this shift, so
	// they do not count on battery power that is about to disappear.
	GridShift float64
}

// Mode returns the decided mode of a unit and whether it was decided
func (d BatteryDecision) Mode(name string) (UnitDecision, bool) {
	i := slices.IndexFunc(d.Units, func(u UnitDecision) bool { return u.Name == name })
	if i < 0 {
		return UnitDecision{}, false
	}
	return d.Units[i], true
}

// unitState is what the battery controller remembers per unit
type unitState struct {
	mode    BatteryMode
	rule    BatteryRule
	changed time.Time

	held bool // held by the reserve rule, released only above reserve plus hysteresis

	anomalySince time.Time // first cycle the unit did not follow its mode
	resends      int
	failed       bool // fell back to normal until restart
}

// BatteryController decides the mode of each battery unit and keeps the state of the rules
// that need memory: export delay, calm, reserve hysteresis, skipped grid charge slots and
// the watchdog.
type BatteryController struct {
	s     BatterySettings
	units map[string]*unitState

	injectionSince time.Time // first cycle of export, zero if not exporting
	injection      bool      // released for export, held until the units stop charging

	skipSlot time.Time // grid charging drew more than planned in this slot
}

// NewBatteryController creates a battery controller
func NewBatteryController(s BatterySettings) *BatteryController {
	return &BatteryController{s: s, units: make(map[string]*unitState)}
}

// SetSettings replaces the settings, keeping the state
func (bc *BatteryController) SetSettings(s BatterySettings) {
	bc.s = s
}

// Settings returns the settings
func (bc *BatteryController) Settings() BatterySettings {
	return bc.s
}

func (bc *BatteryController) unit(name string) *unitState {
	st, ok := bc.units[name]
	if !ok {
		st = &unitState{}
		bc.units[name] = st
	}
	return st
}

// Failed returns the units that fell back to normal until restart
func (bc *BatteryController) Failed() []string {
	var res []string
	for name, st := range bc.units {
		if st.failed {
			res = append(res, name)
		}
	}
	slices.Sort(res)
	return res
}

// updateInjection tracks export: once grid power stays below InjectionPower for InjectionDelay
// the units are released, and stay released while they still charge, so they do not flap
// between hold and normal.
func (bc *BatteryController) updateInjection(in BatteryInput) bool {
	if !in.Measured {
		bc.injectionSince, bc.injection = time.Time{}, false
		return false
	}

	if in.GridPower < bc.s.InjectionPower {
		if bc.injectionSince.IsZero() {
			bc.injectionSince = in.Now
		}
		if in.Now.Sub(bc.injectionSince) >= bc.s.InjectionDelay {
			bc.injection = true
		}
	} else {
		bc.injectionSince = time.Time{}
	}

	if bc.injection && in.GridPower >= bc.s.InjectionPower {
		var charging bool
		for _, u := range in.Units.reachable() {
			if u.Power < -bc.s.WatchdogIdle {
				charging = true
			}
		}
		bc.injection = charging
	}

	return bc.injection
}

// gridChargeUnits returns the units that charge from the grid this cycle: planned for the
// slot, not full, no vehicle charging, and room in the budget for their combined power.
// If both do not fit the largest unit that fits charges alone.
func (bc *BatteryController) gridChargeUnits(in BatteryInput) map[string]bool {
	if !in.Measured || in.Vehicle.Charging || len(in.GridCharge) == 0 || bc.skipSlot.Equal(in.Budget.SlotStart) {
		return nil
	}

	var planned BatteryUnits
	for _, u := range in.Units.reachable() {
		if in.GridCharge[u.Name] && !u.full() && !bc.unit(u.Name).failed {
			planned = append(planned, u)
		}
	}

	// the units already charging are part of grid power: the room is what the house leaves
	room := in.Budget.Rest - in.house()

	// most power first
	slices.SortStableFunc(planned, func(a, b BatteryUnit) int { return cmp.Compare(b.MaxPower, a.MaxPower) })

	var total float64
	for _, u := range planned {
		total += u.MaxPower
	}

	res := make(map[string]bool)
	if total <= room {
		for _, u := range planned {
			res[u.Name] = true
		}
		return res
	}

	for _, u := range planned {
		if u.MaxPower <= room {
			res[u.Name] = true
			return res
		}
	}

	return nil
}

// abortGridCharge skips grid charging for the rest of the slot when the slot is projected above
// target while units charge: the house drew more than planned
func (bc *BatteryController) abortGridCharge(in BatteryInput) bool {
	if !in.Measured || !in.Budget.Exceeded() {
		return false
	}

	for name, st := range bc.units {
		if st.mode == BatteryCharge && st.rule == RuleGridCharge {
			if i := slices.IndexFunc(in.Units, func(u BatteryUnit) bool { return u.Name == name }); i >= 0 && in.Units[i].Reachable {
				bc.skipSlot = in.Budget.SlotStart
				return true
			}
		}
	}

	return false
}

// Decide applies the battery rules to every unit. The first rule that matches wins:
//
//  1. no grid measurement: normal (the meter fallback, no grid charging)
//  2. unit failed in the watchdog: normal until restart
//  3. peak threat: normal, releasing the reserve
//  4. export for a minute, or still charging since: normal
//  5. planned grid charge slot, no vehicle charging, room in the budget: charge
//  6. a vehicle charges: hold at its floor (reserve in smart, buffer or reserve in fast), else normal
//  7. at or below the reserve (plus hysteresis while held): hold
//  8. normal
//
// Except for releasing a peak or a grid charge that draws too much, a unit changes its mode at
// most once per Calm.
func (bc *BatteryController) Decide(in BatteryInput) BatteryDecision {
	injection := bc.updateInjection(in)
	aborted := bc.abortGridCharge(in)
	charge := bc.gridChargeUnits(in)

	var res BatteryDecision

	for _, u := range in.Units {
		st := bc.unit(u.Name)

		reserve := in.Units.share(u, in.Reserve)
		floor := reserve
		if in.Vehicle.Fast && bc.s.FastBattery == FastBatteryBuffer {
			floor = math.Min(reserve, in.Units.share(u, bc.s.Buffer))
		}

		d := UnitDecision{Name: u.Name, Reserve: reserve, Floor: floor}
		urgent := false

		switch {
		case !u.Reachable:
			d.Mode, d.Rule = st.mode, RuleUnreachable
			if d.Mode == "" {
				d.Mode = BatteryNormal
			}
		case !in.Measured:
			d.Mode, d.Rule, urgent = BatteryNormal, RuleMeter, true
		case st.failed:
			d.Mode, d.Rule, urgent = BatteryNormal, RuleWatchdog, true
		case in.Threat:
			d.Mode, d.Rule, urgent = BatteryNormal, RulePeak, true
		case injection:
			d.Mode, d.Rule = BatteryNormal, RuleInjection
		case charge[u.Name]:
			d.Mode, d.Rule = BatteryCharge, RuleGridCharge
		case in.Vehicle.Charging:
			d.Mode, d.Rule = BatteryNormal, RuleVehicle
			if u.Energy() <= floor {
				d.Mode = BatteryHold
			}
		case bc.reserveHold(st, u, reserve):
			d.Mode, d.Rule = BatteryHold, RuleReserve
		default:
			d.Mode, d.Rule = BatteryNormal, RuleNormal
		}

		// a grid charge that draws too much stops at once
		if aborted && st.mode == BatteryCharge {
			urgent = true
		}

		// calm: a unit changes its mode at most once per Calm, unless urgent
		if u.Reachable && st.mode != "" && d.Mode != st.mode && !urgent && in.Now.Sub(st.changed) < bc.s.Calm {
			d.Mode, d.Rule = st.mode, RuleCalm
		}

		st.held = d.Mode == BatteryHold && (d.Rule == RuleReserve || st.held && d.Rule == RuleCalm)

		if u.Reachable {
			d.Changed = d.Mode != st.mode
			if d.Changed {
				res.GridShift += gridShift(u, d.Mode)
				st.mode, st.changed = d.Mode, in.Now
				st.anomalySince = time.Time{}
			}
			if d.Rule != RuleCalm {
				st.rule = d.Rule
			}
		}

		if u.Reachable && d.Mode == BatteryNormal && u.Soc > u.MinSoc+bc.s.AvailableSoc {
			res.Available += u.MaxPower
		}

		res.Units = append(res.Units, d)
	}

	return res
}

// reserveHold reports whether a unit holds for the reserve: at or below its share, or held
// already and not yet above its share plus the hysteresis
func (bc *BatteryController) reserveHold(st *unitState, u BatteryUnit, reserve float64) bool {
	if u.Energy() <= reserve {
		return true
	}
	return st.held && u.Energy() <= reserve+bc.s.Hysteresis/100*u.Capacity
}

// gridShift is the expected change in grid power when unit u switches to mode
func gridShift(u BatteryUnit, mode BatteryMode) float64 {
	switch mode {
	case BatteryHold:
		// a held unit no longer delivers, nor takes up export
		return u.Power
	case BatteryCharge:
		return u.Power + u.MaxPower
	default:
		return 0
	}
}

// WatchdogResult lists the units whose mode must be sent again, and those that failed
type WatchdogResult struct {
	Resend []string // send the mode again
	Failed []string // newly fell back to normal until restart
}

// Watchdog checks that every unit follows the mode it was given. A unit that does not for
// WatchdogDelay gets its mode again; after WatchdogRetries resends it falls back to normal until
// restart. Call it only when the modes are applied, not in shadow mode.
//
//   - normal: import above WatchdogImport, charge above minimum plus WatchdogSoc, and the unit delivers nothing
//   - hold: the unit charges or discharges more than WatchdogHold
//   - charge: the unit does not charge although it is not full
func (bc *BatteryController) Watchdog(in BatteryInput) WatchdogResult {
	var res WatchdogResult

	for _, u := range in.Units {
		st, ok := bc.units[u.Name]
		if !ok || !u.Reachable || st.failed || st.mode == "" {
			continue
		}

		var anomaly bool
		switch st.mode {
		case BatteryNormal:
			anomaly = in.Measured && in.GridPower > bc.s.WatchdogImport && u.Soc > u.MinSoc+bc.s.WatchdogSoc && math.Abs(u.Power) < bc.s.WatchdogIdle
		case BatteryHold:
			anomaly = math.Abs(u.Power) > bc.s.WatchdogHold
		case BatteryCharge:
			anomaly = !u.full() && u.Power > -bc.s.WatchdogIdle
		}

		if !anomaly {
			st.anomalySince = time.Time{}
			continue
		}

		if st.anomalySince.IsZero() {
			st.anomalySince = in.Now
			continue
		}

		if in.Now.Sub(st.anomalySince) < bc.s.WatchdogDelay {
			continue
		}

		st.anomalySince = in.Now
		if st.resends >= bc.s.WatchdogRetries {
			st.failed = true
			res.Failed = append(res.Failed, u.Name)
			continue
		}

		st.resends++
		res.Resend = append(res.Resend, u.Name)
	}

	return res
}

// Reset forgets all state, e.g. when battery control is switched on again
func (bc *BatteryController) Reset() {
	bc.units = make(map[string]*unitState)
	bc.injectionSince, bc.injection = time.Time{}, false
	bc.skipSlot = time.Time{}
}
