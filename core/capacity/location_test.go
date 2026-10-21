package capacity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// The host may run on UTC: months and cooking windows must still follow Belgian time
func TestLocationIndependentOfHost(t *testing.T) {
	assert.Equal(t, "Europe/Brussels", Location.String())

	// 31 October 23:30 UTC is already 1 November 00:30 in Brussels
	now := time.Date(2026, 10, 31, 23, 30, 0, 0, time.UTC)
	assert.True(t, MonthStart(now, Location).Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, Location)))
	assert.True(t, MonthStart(now, Location).Equal(time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC)))

	// Wednesday 21 October 16:30 UTC is 18:30 summer time in Brussels: in the cooking window
	cooking := time.Date(2026, 10, 21, 16, 30, 0, 0, time.UTC)
	assert.False(t, DefaultWindows().Active(cooking), "UTC wall clock is outside the window")
	assert.True(t, DefaultWindows().Active(cooking.In(Location)))

	// after the switch to winter time: 17:30 UTC is 18:30 in Brussels
	winter := time.Date(2026, 10, 28, 17, 30, 0, 0, time.UTC)
	assert.True(t, DefaultWindows().Active(winter.In(Location)))
}
