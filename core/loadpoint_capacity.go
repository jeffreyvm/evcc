package core

import (
	"math"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/capacity"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/util/grid"
)

// capacityController limits loadpoints to the site's capacity tariff budget
type capacityController interface {
	capacityLimit(lp *Loadpoint, st capacity.LoadpointState) (capacity.Decision, bool)
	capacityBatteryAvailable() float64
	GetCapacityMode() string
}

var _ capacityController = (*Site)(nil)

// capacityState returns the loadpoint view for the capacity limiter
func (lp *Loadpoint) capacityState() capacity.LoadpointState {
	phases := max(1, lp.ActivePhases())

	mode := capacity.ModeSmart
	switch lp.GetMode() {
	case api.ModeNow:
		mode = capacity.ModeFast
	case api.ModeOff:
		mode = capacity.ModeOff
	}

	// plans charge like fast mode
	if lp.planActive {
		mode = capacity.ModeFast
	}

	return capacity.LoadpointState{
		Now:              lp.clock.Now().In(capacity.Location),
		Mode:             mode,
		Connected:        lp.connected(),
		ChargePower:      lp.chargePower,
		MinPower:         grid.CurrentToPower(lp.effectiveMinCurrent(), phases),
		MaxPower:         grid.CurrentToPower(lp.effectiveMaxCurrent(), phases),
		BatteryAvailable: lp.capacity.capacityBatteryAvailable(),
	}
}

// applyCapacity caps the requested current to the capacity budget. In shadow mode the
// decision is only published.
func (lp *Loadpoint) applyCapacity(current float64) float64 {
	if lp.capacity == nil {
		return current
	}

	// nothing to limit, also clears a limit published before capacity was switched off
	if current <= 0 || lp.capacity.GetCapacityMode() == CapacityOff {
		lp.publish(keys.CapacityLimited, false)
		lp.publish(keys.CapacityLimitReason, "")
		return current
	}

	d, enforce := lp.capacity.capacityLimit(lp, lp.capacityState())

	lp.publish(keys.CapacityLimited, d.Limited)
	lp.publish(keys.CapacityLimitReason, string(d.Reason))
	lp.publish(keys.CapacityMaxPower, d.MaxPower)
	lp.publish(keys.CapacityEnforced, enforce)

	if !enforce || !d.Limited {
		return current
	}

	// round down: the charger must not exceed the ceiling. The epsilon keeps a ceiling at
	// exactly minimum power from rounding below minimum current after the power round trip.
	limit := grid.PowerToCurrent(d.MaxPower, max(1, lp.ActivePhases()))
	if lp.coarseCurrent() {
		limit = math.Floor(limit + 1e-9)
	} else {
		limit = math.Floor(limit*10+1e-9) / 10
	}

	if limit < lp.effectiveMinCurrent() {
		limit = 0
	}

	if limit < current {
		lp.log.DEBUG.Printf("capacity limit: %.3gA (%s)", limit, d.Reason)
		return limit
	}

	return current
}

// capacityFallback drops a charging loadpoint to minimum current when the grid measurement is lost
func (lp *Loadpoint) capacityFallback() {
	if !lp.enabled {
		return
	}

	if minCurrent := lp.effectiveMinCurrent(); lp.offeredCurrent > minCurrent {
		if err := lp.setLimit(minCurrent); err != nil {
			lp.log.ERROR.Printf("capacity fallback: %v", err)
		}
	}
}

// capacityPlanImmediate reports whether plans start right away. Limited by the capacity
// budget a plan cannot count on full power, so it charges from the start within the budget.
func (lp *Loadpoint) capacityPlanImmediate() bool {
	return lp.capacity != nil && lp.capacity.GetCapacityMode() == CapacityOn
}
