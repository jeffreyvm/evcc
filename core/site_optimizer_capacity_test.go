package core

import (
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/capacity"
	optimizer "github.com/evcc-io/optimizer/client"
	"github.com/stretchr/testify/assert"
)

// quarter hour slots with a flat house load
func capacityTimeSeries(n int, house float32) optimizer.TimeSeries {
	ts := optimizer.TimeSeries{}
	for range n {
		ts.Dt = append(ts.Dt, 900)
		ts.Gt = append(ts.Gt, house/4)
		ts.Ft = append(ts.Ft, 0)
	}
	return ts
}

func vehicleGoal(n, slot int, initial, goal, cmin, cmax float32) optimizerBattery {
	sGoal := make([]float32, n)
	sGoal[slot] = goal
	return optimizerBattery{
		cfg:    optimizer.BatteryConfig{ChargeFromGrid: true, SInitial: initial, SGoal: sGoal, SMax: goal, CMin: cmin, CMax: cmax},
		detail: batteryDetail{Type: batteryTypeLoadpoint, Title: "car"},
	}
}

func TestLimitGoalsToImport(t *testing.T) {
	const eta = 1

	t.Run("reachable goal is kept", func(t *testing.T) {
		// 8 slots of 1.85 kW room = 3.7 kWh
		bats := []optimizerBattery{vehicleGoal(8, 7, 0, 3000, 1400, 7400)}
		res := limitGoalsToImport(capacityTimeSeries(8, 500), bats, 2350, eta)
		assert.Empty(t, res)
		assert.Equal(t, float32(3000), bats[0].cfg.SGoal[7])
	})

	t.Run("unreachable goal is lowered to the room", func(t *testing.T) {
		bats := []optimizerBattery{vehicleGoal(8, 7, 1000, 15000, 1400, 7400)}
		res := limitGoalsToImport(capacityTimeSeries(8, 500), bats, 2350, eta)
		assert.Len(t, res, 1)
		assert.InDelta(t, 1000+8*1850/4, bats[0].cfg.SGoal[7], 1)
	})

	t.Run("minimum power above the room", func(t *testing.T) {
		// a 3p charger at 4.1 kW cannot run within 1.85 kW room
		bats := []optimizerBattery{vehicleGoal(8, 7, 1000, 15000, 4100, 11000)}
		limitGoalsToImport(capacityTimeSeries(8, 500), bats, 2350, eta)
		assert.Equal(t, float32(1000), bats[0].cfg.SGoal[7])
	})

	t.Run("solar adds room", func(t *testing.T) {
		ts := capacityTimeSeries(4, 500)
		for i := range ts.Ft {
			ts.Ft[i] = 1000 // 4 kW
		}
		bats := []optimizerBattery{vehicleGoal(4, 3, 0, 5850, 0, 7400)}
		assert.Empty(t, limitGoalsToImport(ts, bats, 2350, eta))
	})

	t.Run("battery without grid charging stores solar only", func(t *testing.T) {
		ts := capacityTimeSeries(4, 500)
		ts.Ft[1] = 300 // 1.2 kW, 0.7 kW surplus
		bat := vehicleGoal(4, 3, 0, 1000, 0, 2000)
		bat.detail.Type = batteryTypeBattery
		bat.cfg.ChargeFromGrid = false
		bats := []optimizerBattery{bat}
		assert.Len(t, limitGoalsToImport(ts, bats, 2350, eta), 1)
		assert.InDelta(t, 175, bats[0].cfg.SGoal[3], 1)
	})

	t.Run("earlier goal first, room shared", func(t *testing.T) {
		later := vehicleGoal(8, 7, 0, 3700, 0, 7400)
		earlier := vehicleGoal(8, 3, 0, 1850, 0, 7400)
		bats := []optimizerBattery{later, earlier}
		res := limitGoalsToImport(capacityTimeSeries(8, 500), bats, 2350, eta)
		assert.Len(t, res, 1)
		assert.Equal(t, float32(1850), bats[1].cfg.SGoal[3], "earlier kept")
		assert.InDelta(t, 1850, bats[0].cfg.SGoal[7], 1, "later gets the rest")
	})
}

func TestCapacityLimitBatteryMode(t *testing.T) {
	site := &Site{capacityCtl: newSiteCapacity()}
	c := site.capacityCtl
	slot := time.Date(2026, 11, 9, 2, 0, 0, 0, time.UTC)

	// capacity off: the optimizer decides alone
	assert.Equal(t, api.BatteryCharge, site.capacityLimitBatteryMode(api.BatteryCharge, api.BatteryNormal))

	c.mode = CapacityOn
	c.monthPeak.Power = 2500

	// no measurement: no grid charging, self-consumption stores the surplus instead
	assert.Equal(t, api.BatteryUnknown, site.capacityLimitBatteryMode(api.BatteryCharge, api.BatteryNormal), "stays normal")
	assert.Equal(t, api.BatteryNormal, site.capacityLimitBatteryMode(api.BatteryCharge, api.BatteryHold))

	c.valid = true
	c.budget = capacity.Budget{Target: 2350, SlotStart: slot, Rest: 2350}
	c.gridPower = 500
	assert.Equal(t, api.BatteryCharge, site.capacityLimitBatteryMode(api.BatteryCharge, api.BatteryNormal), "room")

	c.gridPower = 2500
	assert.Equal(t, api.BatteryNormal, site.capacityLimitBatteryMode(api.BatteryCharge, api.BatteryHold), "no room")

	// charging continues until the slot is projected above target, then stops for the slot
	assert.Equal(t, api.BatteryUnknown, site.capacityLimitBatteryMode(api.BatteryUnknown, api.BatteryCharge), "within target")
	c.budget.Projection = 1 // 4 kW
	assert.Equal(t, api.BatteryNormal, site.capacityLimitBatteryMode(api.BatteryUnknown, api.BatteryCharge), "above target")
	c.budget.Projection = 0
	c.gridPower = 0
	assert.Equal(t, api.BatteryNormal, site.capacityLimitBatteryMode(api.BatteryCharge, api.BatteryHold), "rest of the slot")

	c.budget.SlotStart = slot.Add(15 * time.Minute)
	assert.Equal(t, api.BatteryCharge, site.capacityLimitBatteryMode(api.BatteryCharge, api.BatteryNormal), "next slot")

	// other modes pass
	assert.Equal(t, api.BatteryNormal, site.capacityLimitBatteryMode(api.BatteryNormal, api.BatteryCharge))
}
