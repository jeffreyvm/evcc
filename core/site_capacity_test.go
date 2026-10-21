package core

import (
	"testing"
	"time"

	"github.com/evcc-io/evcc/core/capacity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	site.updateCapacity(true)
	d, enforce := site.capacityLimit(lp, fast)
	assert.True(t, enforce)
	assert.Equal(t, 1380.0, d.MaxPower)
	assert.Equal(t, capacity.ReasonMeter, d.Reason)
	assert.False(t, site.capacityCtl.fallback)

	// after the meter timeout: fallback, still minimum current
	site.capacityCtl.measured = time.Now().Add(-site.capacityCtl.settings.MeterTimeout)
	site.updateCapacity(true)
	assert.True(t, site.capacityCtl.fallback)

	d, _ = site.capacityLimit(lp, fast)
	assert.Equal(t, 1380.0, d.MaxPower)
	assert.True(t, d.Limited)
}
