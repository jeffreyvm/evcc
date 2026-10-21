package core

import (
	"math"
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/capacity"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestCapacityModeRequiresGridMeter(t *testing.T) {
	site := NewSite()

	require.Error(t, site.SetCapacityMode(CapacityOn), "no grid meter configured")
	assert.Equal(t, CapacityOff, site.GetCapacityMode())
}

// A grid meter that is configured but failed to start must not leave charging unlimited
func TestCapacityGridMeterMissingAtStart(t *testing.T) {
	site := NewSite()
	site.Meters.GridMeterRef = "grid" // configured, no instance
	require.Nil(t, site.gridMeter)

	require.NoError(t, site.SetCapacityMode(CapacityOn))
	assert.Equal(t, CapacityOn, site.GetCapacityMode())

	lp := &Loadpoint{}
	fast := capacity.LoadpointState{
		Now:       time.Now().In(capacity.Location),
		Mode:      capacity.ModeFast,
		Connected: true,
		MinPower:  1380,
		MaxPower:  7360,
	}

	// before the meter timeout: no budget yet, minimum current
	site.updateCapacity(true, 0)
	d, enforce := site.capacityLimit(lp, fast)
	assert.True(t, enforce)
	assert.Equal(t, 1380.0, d.MaxPower)
	assert.Equal(t, capacity.ReasonMeter, d.Reason)
	assert.False(t, site.capacityCtl.fallback)

	// after the meter timeout: fallback, still minimum current
	site.capacityCtl.measured = time.Now().Add(-site.capacityCtl.settings.MeterTimeout)
	site.updateCapacity(true, 0)
	assert.True(t, site.capacityCtl.fallback)

	d, _ = site.capacityLimit(lp, fast)
	assert.Equal(t, 1380.0, d.MaxPower)
	assert.True(t, d.Limited)
}

func TestCapacitySettingBounds(t *testing.T) {
	site := NewSite()

	for _, tc := range []struct {
		floor float64
		ok    bool
	}{
		{2499, false}, // below the billed minimum
		{2500, true},
		{3400, true},
		{25000, true},
		{25001, false},
		{math.NaN(), false},
	} {
		err := site.SetCapacityFloor(tc.floor)
		assert.Equal(t, tc.ok, err == nil, "floor %v: %v", tc.floor, err)
	}
	assert.Equal(t, 25000.0, site.GetCapacityFloor(), "last valid floor")

	for _, tc := range []struct {
		margin float64
		ok     bool
	}{
		{-1, false},
		{0, true},
		{150, true},
		{1000, true},
		{1001, false},
		{math.NaN(), false},
	} {
		err := site.SetCapacityMargin(tc.margin)
		assert.Equal(t, tc.ok, err == nil, "margin %v: %v", tc.margin, err)
	}
	assert.Equal(t, 1000.0, site.GetCapacityMargin(), "last valid margin")

	// any valid margin fits the lowest floor
	require.NoError(t, site.SetCapacityFloor(capacity.MinFloor))
	assert.Equal(t, capacity.MinFloor, site.GetCapacityFloor())
	assert.Equal(t, capacity.MinFloor-capacity.MaxMargin, capacity.Target(site.capacityCtl.settings, 0))
}

// After a crash a battery may still be held: every battery gets normal at start
func TestCapacityBatteryStart(t *testing.T) {
	for _, mode := range []string{CapacityOff, CapacityShadow, CapacityOn} {
		t.Run(mode, func(t *testing.T) {
			ctrl := gomock.NewController(t)

			garage, garageCon := batteryControlMock(ctrl)
			zolder, zolderCon := batteryControlMock(ctrl)

			site := &Site{
				log: util.NewLogger("foo"),
				batteryMeters: []config.Device[api.Meter]{
					config.NewStaticDevice(config.Named{Name: "garage"}, garage),
					config.NewStaticDevice(config.Named{Name: "zolder"}, zolder),
				},
				capacityCtl: newSiteCapacity(),
			}
			site.capacityCtl.mode = mode

			times := 1
			if mode == CapacityOff {
				times = 0
			}
			garageCon.EXPECT().SetBatteryMode(api.BatteryNormal).Times(times)
			zolderCon.EXPECT().SetBatteryMode(api.BatteryNormal).Times(times)

			site.capacityBatteryStart()

			// normal is not sent again on the next regular update
			site.updateBatteryMode(false, false, api.Rate{})

			ctrl.Finish()
		})
	}
}
