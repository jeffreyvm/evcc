package core

import (
	"math"
	"slices"

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
// home batteries covering the house are not counted on, one without grid charging only stores solar.
func limitGoalsToImport(ts optimizer.TimeSeries, batteries []optimizerBattery, limit float64, eta float32) []goalLimit {
	room := make([]float64, len(ts.Dt))
	solar := make([]float64, len(ts.Dt)) // surplus part of the room
	for t, dt := range ts.Dt {
		room[t] = math.Max(0, limit*float64(dt)/3600-float64(at(ts.Gt, t))+float64(at(ts.Ft, t)))
		solar[t] = math.Max(0, float64(at(ts.Ft, t))-float64(at(ts.Gt, t)))
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
			if !cfg.ChargeFromGrid {
				free = math.Min(free, solar[t])
			}
			// a charger that cannot run at its minimum power within the room does not charge at all
			if used[g.bat][t] == 0 && free < float64(cfg.CMin)*hours {
				continue
			}
			take := math.Min(free, need/float64(eta))
			if take <= 0 {
				continue
			}
			room[t] -= take
			solar[t] = math.Max(0, solar[t]-take)
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
