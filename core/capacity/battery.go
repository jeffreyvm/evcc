package capacity

import (
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
	RuleGridCharge  BatteryRule = "gridCharge"       // planned grid charge slot with room in the budget
	RuleNormal      BatteryRule = "normal"           // self-consumption
	RuleMeter       BatteryRule = "meterUnavailable" // no grid measurement: normal, no grid charging
	RuleWatchdog    BatteryRule = "watchdog"         // unit did not follow its mode three times: normal until restart
	RuleUnreachable BatteryRule = "unreachable"      // unit not read this cycle: no command
	RuleCalm        BatteryRule = "calm"             // mode kept, changed less than a minute ago
)

// BatterySettings are the tunable parameters of the battery rules
type BatterySettings struct {
	Calm         time.Duration // minimum time between mode changes of a unit, except leaving grid charging
	AvailableSoc float64       // % soc above the minimum for a unit to count as available

	WatchdogDelay   time.Duration // a unit must misbehave this long before its mode is sent again
	WatchdogImport  float64       // W import above which a unit in normal must deliver
	WatchdogExport  float64       // W export above which a unit in normal must charge
	WatchdogSoc     float64       // % soc above the minimum (below the maximum) for a unit in normal to be expected to deliver (charge)
	WatchdogIdle    float64       // W below which a unit counts as delivering nothing
	WatchdogHold    float64       // W a held unit may still charge or discharge
	WatchdogRetries int           // resends before the unit falls back to normal until restart
}

// DefaultBatterySettings returns the default battery settings
func DefaultBatterySettings() BatterySettings {
	return BatterySettings{
		Calm:            time.Minute,
		AvailableSoc:    2,
		WatchdogDelay:   2 * time.Minute,
		WatchdogImport:  300,
		WatchdogExport:  300,
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
	// ChargePower is the fixed power the unit charges from the grid with (maxchargepower of the
	// meter, which the Indevolt template sends in charge mode), DischargePower its maximum
	// discharge power in self-consumption (maxdischargepower)
	ChargePower    float64 // W
	DischargePower float64 // W
}

// Energy returns the stored energy in kWh
func (u BatteryUnit) Energy() float64 {
	return u.Soc / 100 * u.Capacity
}

// MinEnergy returns the energy below which the unit does not discharge in kWh
func (u BatteryUnit) MinEnergy() float64 {
	return u.MinSoc / 100 * u.Capacity
}

// maxSoc returns the soc in % that ends grid charging
func (u BatteryUnit) maxSoc() float64 {
	if u.MaxSoc <= 0 || u.MaxSoc > 100 {
		return 100
	}
	return u.MaxSoc
}

// full reports whether the unit reached the soc that ends grid charging
func (u BatteryUnit) full() bool {
	return u.Soc >= u.maxSoc()
}

// valid reports whether the unit was read this cycle with a usable soc and capacity
func (u BatteryUnit) valid() bool {
	return u.Reachable && u.Capacity > 0 && !math.IsNaN(u.Soc) && !math.IsInf(u.Soc, 0) && u.Soc >= 0 && u.Soc <= 100 &&
		!math.IsNaN(u.Power) && !math.IsInf(u.Power, 0)
}

// BatteryUnits are the units of a site
type BatteryUnits []BatteryUnit

// reachable returns the units read this cycle
func (uu BatteryUnits) reachable() BatteryUnits {
	var res BatteryUnits
	for _, u := range uu {
		if u.valid() {
			res = append(res, u)
		}
	}
	return res
}

// knownCapacity returns the capacity of all units with a known capacity, also the ones not read
// this cycle, so a short read error does not shift the reserve to the other units
func (uu BatteryUnits) knownCapacity() float64 {
	var res float64
	for _, u := range uu {
		if u.Capacity > 0 && !math.IsNaN(u.Capacity) {
			res += u.Capacity
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

// share returns the part of a site wide energy that falls to unit u, by capacity, never below
// the unit minimum. Units not read this cycle keep their share by their last known capacity.
func (uu BatteryUnits) share(u BatteryUnit, total float64) float64 {
	capacity := uu.knownCapacity()
	if capacity <= 0 || u.Capacity <= 0 {
		return 0
	}
	return math.Max(u.MinEnergy(), total*u.Capacity/capacity)
}

// VehicleState is what the battery rules need to know about charging vehicles
type VehicleState struct {
	// Charging: a loadpoint charges, or is connected and wants to charge regardless of solar
	// (fast, plan, minimum soc), also while the capacity limit pauses it
	Charging bool
}

// BatteryInput is the state of one control cycle for the battery rules
type BatteryInput struct {
	Now       time.Time
	Units     BatteryUnits
	Measured  bool    // budget and grid power are based on a fresh measurement
	Budget    Budget  // slot budget, valid if Measured
	GridPower float64 // W, positive is import
	Vehicle   VehicleState

	Reserve    float64         // kWh site reserve target incl. the unit minimums
	GridCharge map[string]bool // units planned to charge from the grid in the current slot
}

// house returns the consumption without the batteries in W: grid power plus what they deliver.
// A unit not read this cycle is counted at 0.
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
	Changed bool        `json:"-"`       // mode differs from the last decision
}

// BatteryDecision is the result of the battery rules for one cycle
type BatteryDecision struct {
	Units []UnitDecision

	// Available is the power in W the units in normal can deliver above their minimum, for an
	// appliance switching on. A unit at its minimum, held or charging delivers nothing.
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

	anomalySince time.Time // first cycle the unit did not follow its mode
	resends      int
	failed       bool // fell back to normal until restart
}

// BatteryController decides the mode of each battery unit and keeps the state of the rules
// that need memory: calm, skipped grid charge slots and the watchdog.
type BatteryController struct {
	s     BatterySettings
	units map[string]*unitState

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

	res := make(map[string]bool)
	for _, u := range fitUnits(planned, room, func(u BatteryUnit) float64 { return u.ChargePower }) {
		res[u.Name] = true
	}
	return res
}

// abortGridCharge skips grid charging for the rest of the slot when the slot is projected above
// target while units charge: the house drew more than planned
func (bc *BatteryController) abortGridCharge(in BatteryInput) {
	if !in.Measured || !in.Budget.Exceeded() {
		return
	}

	for _, u := range in.Units.reachable() {
		if st, ok := bc.units[u.Name]; ok && st.mode == BatteryCharge && st.rule == RuleGridCharge {
			bc.skipSlot = in.Budget.SlotStart
			return
		}
	}
}

// Decide applies the battery rules to every unit. The units stay in self-consumption and
// leave it only to charge from the grid. The first rule that matches wins:
//
//  1. no grid measurement: normal (the meter fallback, no grid charging)
//  2. unit failed in the watchdog: normal until restart
//  3. planned grid charge slot, no vehicle charging, room in the budget: charge, and the other
//     units hold so they do not discharge into the charging one
//  4. normal
//
// A unit changes its mode at most once per Calm, except for rules 1 and 2 and leaving grid charging.
// A unit not read this cycle gets no new mode, except normal if it was charging from the grid.
func (bc *BatteryController) Decide(in BatteryInput) BatteryDecision {
	bc.abortGridCharge(in)
	charge := bc.gridChargeUnits(in)

	// the other units hold only while one actually charges, not while calm delays it
	charging := false
	for name := range charge {
		st := bc.unit(name)
		charging = charging || st.mode == "" || st.mode == BatteryCharge || in.Now.Sub(st.changed) >= bc.s.Calm
	}

	var res BatteryDecision

	for _, u := range in.Units {
		st := bc.unit(u.Name)

		d := UnitDecision{Name: u.Name, Reserve: in.Units.share(u, in.Reserve)}

		if !u.valid() {
			d.Mode, d.Rule = st.mode, RuleUnreachable
			switch st.mode {
			case "":
				d.Mode = BatteryNormal
			case BatteryCharge:
				// best effort: a unit charging from the grid must not go on unseen
				d.Mode, d.Changed = BatteryNormal, true
				st.mode, st.changed, st.anomalySince = BatteryNormal, in.Now, time.Time{}
			}
			res.Units = append(res.Units, d)
			continue
		}

		urgent := false

		switch {
		case !in.Measured:
			d.Mode, d.Rule, urgent = BatteryNormal, RuleMeter, true
		case st.failed:
			d.Mode, d.Rule, urgent = BatteryNormal, RuleWatchdog, true
		case charge[u.Name]:
			d.Mode, d.Rule = BatteryCharge, RuleGridCharge
		case charging:
			d.Mode, d.Rule = BatteryHold, RuleGridCharge
		default:
			d.Mode, d.Rule = BatteryNormal, RuleNormal
		}

		// leaving grid charging never waits: the vehicle or the house need the room
		if st.mode == BatteryCharge && d.Mode != BatteryCharge {
			urgent = true
		}

		// calm: a unit changes its mode at most once per Calm, unless urgent
		if st.mode != "" && d.Mode != st.mode && !urgent && in.Now.Sub(st.changed) < bc.s.Calm {
			d.Mode, d.Rule = st.mode, RuleCalm
		}

		d.Changed = d.Mode != st.mode
		if d.Changed {
			res.GridShift += gridShift(u, d.Mode)
			st.mode, st.changed = d.Mode, in.Now
			st.anomalySince = time.Time{}
		}
		if d.Rule != RuleCalm {
			st.rule = d.Rule
		}

		// what the unit can still deliver for an appliance on top of what it delivers now
		if d.Mode == BatteryNormal && u.Soc > u.MinSoc+bc.s.AvailableSoc {
			res.Available += math.Max(0, u.DischargePower-math.Max(0, u.Power))
		}

		res.Units = append(res.Units, d)
	}

	return res
}

// gridShift is the expected change in grid power when unit u switches to mode
func gridShift(u BatteryUnit, mode BatteryMode) float64 {
	switch mode {
	case BatteryHold:
		// a held unit no longer delivers, nor takes up export
		return u.Power
	case BatteryCharge:
		return u.Power + u.ChargePower
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
//   - normal: import above WatchdogImport, charge above minimum plus WatchdogSoc, and the unit delivers nothing,
//     or export above WatchdogExport, charge below its maximum minus WatchdogSoc, and the unit takes nothing
//   - hold: the unit discharges more than WatchdogHold. Charging from solar in hold is allowed.
//   - charge: the unit does not charge although it is not full
//
// Resends count only while the unit keeps misbehaving: once it follows again they start afresh.
func (bc *BatteryController) Watchdog(in BatteryInput) WatchdogResult {
	var res WatchdogResult

	for _, u := range in.Units {
		st, ok := bc.units[u.Name]
		if !ok || !u.valid() || st.failed || st.mode == "" {
			continue
		}

		var anomaly bool
		switch st.mode {
		case BatteryNormal:
			idle := math.Abs(u.Power) < bc.s.WatchdogIdle
			importing := in.GridPower > bc.s.WatchdogImport && u.Soc > u.MinSoc+bc.s.WatchdogSoc
			exporting := in.GridPower < -bc.s.WatchdogExport && u.Soc < u.maxSoc()-bc.s.WatchdogSoc
			anomaly = in.Measured && idle && (importing || exporting)
		case BatteryHold:
			anomaly = u.Power > bc.s.WatchdogHold
		case BatteryCharge:
			anomaly = !u.full() && u.Power > -bc.s.WatchdogIdle
		}

		if !anomaly {
			st.anomalySince, st.resends = time.Time{}, 0
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
	*bc = *NewBatteryController(bc.s)
}
