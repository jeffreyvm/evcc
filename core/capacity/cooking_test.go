package capacity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseWindows(t *testing.T) {
	for _, tc := range []struct {
		in  string
		res []Window
	}{
		{"", nil},
		{"none", nil},
		{"17:00-19:00", []Window{{From: 17 * time.Hour, To: 19 * time.Hour}}},
		{" 17:30-19:00 , 06:00-07:15 ", []Window{
			{From: 6 * time.Hour, To: 7*time.Hour + 15*time.Minute},
			{From: 17*time.Hour + 30*time.Minute, To: 19 * time.Hour},
		}},
		{"22:00-24:00", []Window{{From: 22 * time.Hour, To: 24 * time.Hour}}},
		{"7:00-8:00", []Window{{From: 7 * time.Hour, To: 8 * time.Hour}}},
	} {
		res, err := ParseWindows(tc.in)
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.res, res, tc.in)
	}

	for _, in := range []string{
		"17-19", "17:00", "19:00-17:00", "17:00-17:00", "24:30-25:00", "17:60-18:00",
		"17:0-18:00", "-1:00-02:00", "a:00-b:00", "17:00-19:00,", "01:00-02:00,03:00-04:00,05:00-06:00,07:00-08:00,09:00-10:00",
	} {
		_, err := ParseWindows(in)
		assert.Error(t, err, in)
	}
}

func TestFormatWindows(t *testing.T) {
	assert.Equal(t, "", FormatWindows(nil))
	ws := DefaultWindows()
	assert.Equal(t, "17:00-19:00", FormatWindows(ws.Weekday))
	assert.Equal(t, "10:30-12:00", FormatWindows(ws.Weekend))

	for _, s := range []string{"06:00-07:15,17:30-19:00", "22:00-24:00"} {
		res, err := ParseWindows(s)
		require.NoError(t, err)
		assert.Equal(t, s, FormatWindows(res))
	}
}

// Switched off, no window is active, but the configured ones are kept for switching on again
func TestWindowsOff(t *testing.T) {
	ws := DefaultWindows()
	tue := time.Date(2026, 11, 3, 17, 30, 0, 0, Location)
	sat := time.Date(2026, 11, 7, 11, 0, 0, 0, Location)
	assert.True(t, ws.Active(tue))
	assert.True(t, ws.Active(sat))

	ws.Off = true
	assert.False(t, ws.Active(tue))
	assert.False(t, ws.Active(sat))
	assert.Len(t, ws.Weekday, 1)

	ws = Windows{Weekday: []Window{{From: 6 * time.Hour, To: 7 * time.Hour}}}
	assert.True(t, ws.Active(time.Date(2026, 11, 3, 6, 30, 0, 0, Location)))
	assert.False(t, ws.Active(tue), "only the configured window")
	assert.False(t, ws.Active(sat), "no weekend window")
}
