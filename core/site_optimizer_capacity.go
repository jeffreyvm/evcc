package core

import (
	"math"
	"slices"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/capacity"
	optimizer "github.com/evcc-io/optimizer/client"
)

// goalLimit is a charge goal lowered to what fits within the grid import limit
type goalLimit struct {
	title      string
	goal, kept float64 // Wh
}

// limitGoalsToImport lowers charge goals to what the batteries can store within the grid import
// limit, earliest goal first. The optimizer prices a missed goal like import above the limit, so
// an unreachable goal would plan import above it. House consumption and solar are as forecast,
// home batteries covering the house are not counted on.
func limitGoalsToImport(ts optimizer.TimeSeries, batteries []optimizerBattery, limit float64, eta float32) []goalLimit {
	room := make([]float64, len(ts.Dt))
	for t, dt := range ts.Dt {
		room[t] = math.Max(0, limit*float64(dt)/3600-float64(at(ts.Gt, t))+float64(at(ts.Ft, t)))
	}

	type goal struct{ bat, slot int }
	var goals []goal
	for i, b := range batteries {
		for t, g := range b.cfg.SGoal {
			if g > 0 && t < len(room) {
				goals = append(goals, goal{i, t})
			}
		}
	}
	slices.SortStableFunc(goals, func(a, b goal) int { return a.slot - b.slot })

	stored := make([]float64, len(batteries)) // Wh stored per battery so far
	used := make([][]float64, len(batteries)) // Wh drawn per battery and slot
	for i := range used {
		used[i] = make([]float64, len(room))
	}

	var res []goalLimit
	for _, g := range goals {
		cfg := &batteries[g.bat].cfg
		need := float64(cfg.SGoal[g.slot]) - float64(cfg.SInitial) - stored[g.bat]
		if need <= 0 {
			continue
		}

		for t := 0; t <= g.slot && need > 1e-6; t++ {
			hours := float64(ts.Dt[t]) / 3600
			free := math.Min(room[t], float64(cfg.CMax)*hours-used[g.bat][t])
			// a charger that cannot run at its minimum power within the room does not charge at all
			if used[g.bat][t] == 0 && free < float64(cfg.CMin)*hours {
				continue
			}
			take := math.Min(free, need/float64(eta))
			if take <= 0 {
				continue
			}
			room[t] -= take
			used[g.bat][t] += take
			stored[g.bat] += take * float64(eta)
			need -= take * float64(eta)
		}

		if need > 1 {
			goalWh := float64(cfg.SGoal[g.slot])
			kept := float64(cfg.SInitial) + stored[g.bat]
			cfg.SGoal[g.slot] = float32(kept)
			res = append(res, goalLimit{title: batteries[g.bat].detail.Title, goal: goalWh, kept: kept})
		}
	}

	return res
}

func at(s []float32, i int) float32 {
	if i < len(s) {
		return s[i]
	}
	return 0
}

// addCapacityReserveGoals sets the capacity reserve as goal of the home batteries at its deadline,
// so the optimizer fills them from solar first and from the grid within the capacity target. The
// reserve follows the expected house peaks above the target or the season floor.
func (site *Site) addCapacityReserveGoals(batteries []optimizerBattery, now time.Time, minLen int) {
	c := site.capacityCtl
	if c == nil {
		return
	}

	c.mu.Lock()
	rs := c.bat.reserve
	target := capacity.Target(c.settings, c.monthPeak.Power)
	c.mu.Unlock()

	const n = 2 * 96 // two days cover the deadline of tomorrow
	p90, p50, days, err := site.capacityHouseProfiles(now, n)
	if err != nil {
		days = 0
	}

	units := site.capacityBatteryUnits()
	plan := rs.PlanGridCharge(capacity.ReserveInput{
		Now:         now.In(capacity.Location),
		Units:       units,
		Target:      target,
		HouseP90:    p90,
		HouseP50:    p50,
		ProfileDays: days,
		Solar:       site.capacitySolar(n, now),
		SolarScale:  site.effectiveSolarScale(),
	})

	slot := planSlot(now, plan.Deadline)
	if slot < 0 || slot >= minLen {
		return
	}

	for i := range batteries {
		b := &batteries[i]
		if b.detail.Type != batteryTypeBattery || !b.cfg.ChargeFromGrid {
			continue
		}
		j := slices.IndexFunc(units, func(u capacity.BatteryUnit) bool { return u.Name == b.detail.Name })
		if j < 0 {
			continue
		}
		goal := min(float32(units.Share(units[j], plan.Reserve)*1e3), b.cfg.SMax)
		if goal <= b.cfg.SMin {
			continue
		}
		setPlanGoal(&b.cfg, minLen, slot, float64(goal))
		site.log.DEBUG.Printf("optimizer: battery %s reserve %.2f kWh by %s", b.detail.Title, goal/1e3, plan.Deadline.In(capacity.Location).Format("15:04"))
	}
}

// capacityGridCharge decides whether the home batteries may charge from the grid in the current
// cycle while the capacity target is enforced. They charge at a fixed power, so they only start
// with room in the budget for it and stop for the rest of the slot once the slot is projected
// above target.
func (site *Site) capacityGridCharge(charging bool) bool {
	if site.capacityImportLimit() <= 0 {
		return true
	}

	units := site.capacityBatteryUnits()

	c := site.capacityCtl
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.valid || c.fallback {
		return false
	}

	b := c.budget
	if c.bat.optimizerSkip.Equal(b.SlotStart) {
		return false
	}

	if charging {
		if b.Exceeded() {
			c.bat.optimizerSkip = b.SlotStart
			return false
		}
		return true
	}

	// the house without the batteries: grid power plus what they deliver
	house := c.gridPower
	var power float64
	for _, u := range units {
		if u.Reachable {
			house += u.Power
		}
		power += u.ChargePower
	}

	return b.Rest-house >= power
}

// capacityLimitBatteryMode keeps an optimizer grid charge within the capacity budget: hold instead
func (site *Site) capacityLimitBatteryMode(res, current api.BatteryMode) api.BatteryMode {
	charge := res == api.BatteryCharge || res == api.BatteryUnknown && current == api.BatteryCharge
	if !charge || site.capacityGridCharge(current == api.BatteryCharge) {
		return res
	}
	if current == api.BatteryHold {
		return api.BatteryUnknown
	}
	return api.BatteryHold
}
