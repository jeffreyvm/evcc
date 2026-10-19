package capacity

import "time"

// Mode is the charge mode as far as the capacity rules distinguish it
type Mode int

const (
	ModeSmart Mode = iota // solar, smart and plan driven charging
	ModeFast              // charge as fast as the budget allows
	ModeOff               // not charging by intent
)

// Reason explains why a loadpoint is limited
type Reason string

const (
	ReasonNone           Reason = ""
	ReasonBudget         Reason = "budget"         // quarter-hour budget
	ReasonMinCurrentHold Reason = "minCurrentHold" // held at minimum current although the budget is smaller
	ReasonPaused         Reason = "paused"         // stopped, waiting for enough headroom to restart
	ReasonCooking        Reason = "cookingWindow"  // restricted in the cooking window
	ReasonMeter          Reason = "meterUnavailable"
)

// LoadpointState is the loadpoint view the limiter needs
type LoadpointState struct {
	Now       time.Time // in the site's local time zone, used for the cooking window
	Mode      Mode
	Connected bool

	ChargePower float64 // W, measured
	MinPower    float64 // W at minimum current and active phases
	MaxPower    float64 // W at maximum current and active phases

	BatteryAvailable float64 // W the home battery can deliver above its minimum soc
}

// Decision is the power ceiling for a loadpoint
type Decision struct {
	MaxPower float64 // W ceiling, 0 means stop
	Limited  bool    // ceiling is below the loadpoint maximum
	Reason   Reason
	Cooking  bool // a cooking window is active
}

// Limiter derives the power ceiling for one loadpoint and keeps the
// hysteresis state below minimum current
type Limiter struct {
	s Settings

	paused        bool
	pausedSince   time.Time
	headroomSince time.Time // first cycle with enough headroom while paused
}

// NewLimiter creates a limiter for one loadpoint
func NewLimiter(s Settings) *Limiter {
	return &Limiter{s: s}
}

// SetSettings replaces the settings, keeping the hysteresis state
func (l *Limiter) SetSettings(s Settings) {
	l.s = s
}

// Paused reports whether the limiter currently holds the loadpoint stopped
func (l *Limiter) Paused() bool {
	return l.paused
}

func (l *Limiter) reset() {
	l.paused = false
	l.pausedSince = time.Time{}
	l.headroomSince = time.Time{}
}

// Limit returns the power ceiling for the loadpoint. The ceiling is the loadpoint's own
// power plus what is still free in the slot: Rest minus grid power. Export is negative
// grid power, so solar surplus adds to the ceiling by itself.
func (l *Limiter) Limit(b Budget, gridPower float64, st LoadpointState) Decision {
	if !st.Connected || st.Mode == ModeOff {
		l.reset()
		return Decision{MaxPower: st.MaxPower}
	}

	ceiling := st.ChargePower + b.Rest - gridPower
	reason := ReasonBudget
	offGrid := false

	_, cooking := l.s.Cooking.Active(st.Now)
	if cooking {
		switch st.Mode {
		case ModeFast:
			// fast charges at minimum current, an appliance switching on must fit the slot
			if ceiling > st.MinPower {
				ceiling, reason = st.MinPower, ReasonCooking
			}
		default:
			// smart stays off grid unless the battery can cover an appliance
			if st.BatteryAvailable < l.s.CookingBattery {
				// own power minus grid import is what solar covers, export adds to it
				if surplus := st.ChargePower - gridPower; surplus < ceiling {
					ceiling, reason, offGrid = surplus, ReasonCooking, true
				}
			}
		}
	}

	if l.paused {
		if !l.restart(ceiling, st) {
			return Decision{Limited: true, Reason: ReasonPaused, Cooking: cooking}
		}
	}

	if ceiling >= st.MinPower {
		if ceiling >= st.MaxPower {
			return Decision{MaxPower: st.MaxPower, Cooking: cooking}
		}
		return Decision{MaxPower: ceiling, Limited: true, Reason: reason, Cooking: cooking}
	}

	// below minimum current: stay at minimum while the slot still ends below target,
	// so short shortfalls do not toggle the charger
	charging := st.ChargePower > 0
	if charging && !offGrid && b.HoldsAt(gridPower, st.MinPower-st.ChargePower) {
		return Decision{MaxPower: st.MinPower, Limited: true, Reason: ReasonMinCurrentHold, Cooking: cooking}
	}

	l.paused = true
	l.pausedSince = st.Now
	l.headroomSince = time.Time{}

	return Decision{Limited: true, Reason: reason, Cooking: cooking}
}

// restart reports whether a paused loadpoint may start again: the ceiling has stayed
// RestartHeadroom above minimum power for MinCurrentHold and the pause lasted RestartPause
func (l *Limiter) restart(ceiling float64, st LoadpointState) bool {
	if ceiling < st.MinPower+l.s.RestartHeadroom {
		l.headroomSince = time.Time{}
		return false
	}

	if l.headroomSince.IsZero() {
		l.headroomSince = st.Now
	}

	if st.Now.Sub(l.headroomSince) < l.s.MinCurrentHold || st.Now.Sub(l.pausedSince) < l.s.RestartPause {
		return false
	}

	l.reset()
	return true
}

// Fallback is the decision when the grid measurement is unavailable: minimum current
func Fallback(st LoadpointState) Decision {
	if !st.Connected || st.Mode == ModeOff {
		return Decision{MaxPower: st.MaxPower}
	}
	return Decision{MaxPower: st.MinPower, Limited: true, Reason: ReasonMeter}
}
