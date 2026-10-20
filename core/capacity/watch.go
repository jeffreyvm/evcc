package capacity

import "time"

// PeakWatch raises a peak threat when the slot is projected above target and grid
// power has stayed above the remaining budget for at least ThreatDelay.
// The delay filters short spikes from a kettle or oven element.
type PeakWatch struct {
	delay time.Duration
	since time.Time // first cycle with grid power above budget, zero if not
}

// NewPeakWatch creates a peak watch with the given debounce delay
func NewPeakWatch(delay time.Duration) *PeakWatch {
	return &PeakWatch{delay: delay}
}

// Update evaluates the current cycle and reports whether a peak threatens
func (w *PeakWatch) Update(b Budget, gridPower float64, now time.Time) bool {
	if gridPower <= b.Rest {
		w.since = time.Time{}
		return false
	}

	// a new slot starts with a fresh budget
	if !w.since.IsZero() && !SlotStart(w.since).Equal(b.SlotStart) {
		w.since = time.Time{}
	}

	if w.since.IsZero() {
		w.since = now
	}

	return b.Exceeded() && now.Sub(w.since) >= w.delay
}

// Reset clears the debounce state, e.g. after a measurement gap
func (w *PeakWatch) Reset() {
	w.since = time.Time{}
}
