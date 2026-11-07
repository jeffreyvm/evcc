package core

import (
	"errors"
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

// After a crash a battery may still be held: with battery control on every battery gets normal
// at start. Without battery control evcc sent no mode, so it sends none at start either.
func TestCapacityBatteryStart(t *testing.T) {
	for _, tc := range []struct{ mode, battery string }{
		{CapacityOff, CapacityOn},
		{CapacityShadow, CapacityOn},
		{CapacityOn, CapacityOff},
		{CapacityOn, CapacityShadow},
	} {
		mode := tc.mode
		t.Run(mode+"/"+tc.battery, func(t *testing.T) {
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
			site.capacityCtl.bat.mode = tc.battery

			times := 1
			if mode == CapacityOff || tc.battery != CapacityOn {
				times = 0
			}
			garageCon.EXPECT().SetBatteryMode(api.BatteryNormal).Times(times)
			zolderCon.EXPECT().SetBatteryMode(api.BatteryNormal).Times(times)

			site.capacityBatteryStart()
			site.capacityBatteryNormal()

			// normal is not sent again on the next regular update
			site.capacityBatteryNormal()
			site.updateBatteryMode(false, false, api.Rate{})

			ctrl.Finish()
		})
	}
}

// A battery not reachable at start is tried again every cycle, without resending normal
// to the battery that already accepted it
func TestCapacityBatteryStartRetry(t *testing.T) {
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
	site.capacityCtl.mode = CapacityShadow
	site.capacityCtl.bat.mode = CapacityOn

	gomock.InOrder(
		garageCon.EXPECT().SetBatteryMode(api.BatteryNormal).Return(errors.New("booting")),
		garageCon.EXPECT().SetBatteryMode(api.BatteryNormal).Return(errors.New("booting")),
		garageCon.EXPECT().SetBatteryMode(api.BatteryNormal),
	)
	zolderCon.EXPECT().SetBatteryMode(api.BatteryNormal).Times(1)

	site.capacityBatteryStart()

	site.capacityBatteryNormal()
	assert.Equal(t, api.BatteryUnknown, site.GetBatteryMode(), "garage not reachable")
	site.capacityBatteryNormal()
	assert.Equal(t, api.BatteryUnknown, site.GetBatteryMode(), "garage still not reachable")
	site.capacityBatteryNormal()
	assert.Equal(t, api.BatteryNormal, site.GetBatteryMode(), "both normal")

	site.capacityBatteryNormal()
	site.updateBatteryMode(false, false, api.Rate{})
}

// A battery held by evcc or an external system does not cover an appliance
func TestCapacityBatteryAvailableHeld(t *testing.T) {
	ctrl := gomock.NewController(t)
	bat, _ := batteryControlMock(ctrl)

	site := NewSite()
	site.batteryMeters = []config.Device[api.Meter]{config.NewStaticDevice(config.Named{Name: "bat"}, bat)}
	site.siteState.battery.Soc = 80

	assert.Equal(t, 2000.0, site.capacityBatteryAvailable())

	for _, mode := range []api.BatteryMode{api.BatteryHold, api.BatteryCharge} {
		site.batteryMode = mode
		assert.Zero(t, site.capacityBatteryAvailable(), "%s", mode)
	}
	site.batteryMode = api.BatteryNormal

	site.batteryModeExternal = api.BatteryHold
	assert.Zero(t, site.capacityBatteryAvailable(), "external hold")
}

// Charging loadpoints share the free budget instead of each claiming all of it
func TestCapacitySharedBudget(t *testing.T) {
	c := newSiteCapacity()
	c.budget = capacity.Budget{Rest: 2350}
	c.limitPower = 1350 // free 1000W

	charging := capacity.LoadpointState{ChargePower: 1380}
	waiting := capacity.LoadpointState{}

	c.charging = 1
	assert.Equal(t, 2350.0, c.shared(charging).Rest, "single loadpoint keeps the whole budget")
	assert.Equal(t, 1350.0+500, c.shared(waiting).Rest, "a second one asking shares it")

	c.charging = 2
	assert.Equal(t, 1350.0+500, c.shared(charging).Rest, "two charging")

	// over budget, each one gives up its share of the excess
	c.limitPower = 3350
	assert.Equal(t, 3350.0-500, c.shared(charging).Rest)
}

func TestCapacityCookingSettings(t *testing.T) {
	site := NewSite()

	assert.True(t, site.GetCapacityCooking())
	assert.Equal(t, "17:00-19:00", site.GetCapacityCookingWeekday())
	assert.Equal(t, "10:30-12:00", site.GetCapacityCookingWeekend())

	require.NoError(t, site.SetCapacityCookingWeekday("06:30-07:30,17:30-19:30"))
	assert.Equal(t, "06:30-07:30,17:30-19:30", site.GetCapacityCookingWeekday())
	require.NoError(t, site.SetCapacityCookingWeekend("none"))
	assert.Equal(t, "", site.GetCapacityCookingWeekend())

	assert.Error(t, site.SetCapacityCookingWeekday("19:00-17:00"))
	assert.Equal(t, "06:30-07:30,17:30-19:30", site.GetCapacityCookingWeekday(), "invalid windows change nothing")

	// a limiter created before the change follows it
	lp := &Loadpoint{}
	l := site.capacityCtl.limiter(lp)
	st := capacity.LoadpointState{
		Now:       time.Date(2026, 11, 3, 18, 0, 0, 0, capacity.Location),
		Mode:      capacity.ModeFast,
		Connected: true,
		MinPower:  1380,
		MaxPower:  7360,
	}
	b := capacity.Budget{Target: 2350, SlotStart: capacity.SlotStart(st.Now), Remaining: 15 * time.Minute, Rest: 2350}
	assert.True(t, l.Limit(b, 500, st).Cooking)

	require.NoError(t, site.SetCapacityCooking(false))
	assert.False(t, site.GetCapacityCooking())
	assert.False(t, l.Limit(b, 500, st).Cooking, "switched off")
	assert.Equal(t, "06:30-07:30,17:30-19:30", site.GetCapacityCookingWeekday(), "windows kept while off")

	require.NoError(t, site.SetCapacityCooking(true))
	assert.True(t, l.Limit(b, 500, st).Cooking)
}

func TestCapacityImportLimit(t *testing.T) {
	assert.Zero(t, new(Site).capacityImportLimit(), "no capacity control")

	site := &Site{capacityCtl: newSiteCapacity()}
	site.capacityCtl.monthPeak.Power = 4000

	for _, mode := range []string{CapacityOff, CapacityShadow} {
		site.capacityCtl.mode = mode
		assert.Zero(t, site.capacityImportLimit(), mode)
	}

	site.capacityCtl.mode = CapacityOn
	assert.Equal(t, capacity.Target(site.capacityCtl.settings, 4000), site.capacityImportLimit())
}
