package capacity

import "time"

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
	Weekday []Window
	Weekend []Window
}

// DefaultWindows returns the starting values from the design: weekdays 17:00-19:00, weekends 10:30-12:00
func DefaultWindows() Windows {
	return Windows{
		Weekday: []Window{{From: 17 * time.Hour, To: 19 * time.Hour}},
		Weekend: []Window{{From: 10*time.Hour + 30*time.Minute, To: 12 * time.Hour}},
	}
}

// Active returns the window containing t, if any. t must be in the site's local time zone.
func (ws Windows) Active(t time.Time) (Window, bool) {
	list := ws.Weekday
	if wd := t.Weekday(); wd == time.Saturday || wd == time.Sunday {
		list = ws.Weekend
	}
	for _, w := range list {
		if w.Contains(t) {
			return w, true
		}
	}
	return Window{}, false
}

// sinceMidnight returns the wall clock time of day of t, independent of DST transitions
func sinceMidnight(t time.Time) time.Duration {
	h, m, s := t.Clock()
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second
}
