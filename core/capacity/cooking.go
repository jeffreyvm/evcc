package capacity

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// MaxWindows is the most cooking windows per day type
const MaxWindows = 4

// Window is a daily time range in local time, [From, To)
type Window struct {
	From, To time.Duration // since local midnight
}

// Contains reports whether the local clock time of t lies in the window
func (w Window) Contains(t time.Time) bool {
	since := sinceMidnight(t)
	return w.From <= since && since < w.To
}

// Windows are the cooking windows on weekdays and weekends.
// In a cooking window an appliance switching on can push the slot above target
// while the vehicle has already used the budget, so charging is restricted.
type Windows struct {
	Off     bool // switched off: no window is active, the configured ones are kept
	Weekday []Window
	Weekend []Window
}

// DefaultWindows returns the default cooking windows: weekdays 17:00-19:00, weekends 10:30-12:00
func DefaultWindows() Windows {
	return Windows{
		Weekday: []Window{{From: 17 * time.Hour, To: 19 * time.Hour}},
		Weekend: []Window{{From: 10*time.Hour + 30*time.Minute, To: 12 * time.Hour}},
	}
}

// Active reports whether t lies in a cooking window. t must be in Location.
func (ws Windows) Active(t time.Time) bool {
	if ws.Off {
		return false
	}
	list := ws.Weekday
	if wd := t.Weekday(); wd == time.Saturday || wd == time.Sunday {
		list = ws.Weekend
	}
	return slices.ContainsFunc(list, func(w Window) bool { return w.Contains(t) })
}

// sinceMidnight returns the wall clock time of day of t, independent of DST transitions
func sinceMidnight(t time.Time) time.Duration {
	h, m, s := t.Clock()
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second
}

// ParseWindows parses cooking windows as "HH:MM-HH:MM", comma separated, in local time.
// An empty string or "none" is no window. A window ends at 24:00 at the latest.
func ParseWindows(s string) ([]Window, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "none" {
		return nil, nil
	}

	var res []Window
	for part := range strings.SplitSeq(s, ",") {
		from, to, ok := strings.Cut(strings.TrimSpace(part), "-")
		if !ok {
			return nil, fmt.Errorf("invalid cooking window: %q, expected HH:MM-HH:MM", part)
		}
		f, err := parseClock(from)
		if err != nil {
			return nil, err
		}
		t, err := parseClock(to)
		if err != nil {
			return nil, err
		}
		if f >= t {
			return nil, fmt.Errorf("invalid cooking window: %q, must end after it starts", part)
		}
		res = append(res, Window{From: f, To: t})
	}

	if len(res) > MaxWindows {
		return nil, fmt.Errorf("too many cooking windows: %d, at most %d", len(res), MaxWindows)
	}

	slices.SortFunc(res, func(a, b Window) int { return int(a.From - b.From) })
	return res, nil
}

// FormatWindows formats cooking windows as ParseWindows reads them, empty for none
func FormatWindows(ws []Window) string {
	res := make([]string, 0, len(ws))
	for _, w := range ws {
		res = append(res, formatClock(w.From)+"-"+formatClock(w.To))
	}
	return strings.Join(res, ",")
}

var errClock = errors.New("expected HH:MM")

// parseClock parses HH:MM from 00:00 to 24:00 into the time since midnight
func parseClock(s string) (time.Duration, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return 0, fmt.Errorf("invalid time %q: %w", s, errClock)
	}
	hh, err := strconv.Atoi(h)
	if err != nil || len(h) > 2 {
		return 0, fmt.Errorf("invalid time %q: %w", s, errClock)
	}
	mm, err := strconv.Atoi(m)
	if err != nil || len(m) != 2 {
		return 0, fmt.Errorf("invalid time %q: %w", s, errClock)
	}
	if hh < 0 || mm < 0 || mm > 59 || hh > 24 || hh == 24 && mm > 0 {
		return 0, fmt.Errorf("invalid time %q: %w", s, errClock)
	}
	return time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute, nil
}

func formatClock(d time.Duration) string {
	m := int(d / time.Minute)
	return fmt.Sprintf("%02d:%02d", m/60, m%60)
}
