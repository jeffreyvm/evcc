package capacity

import (
	"fmt"
	"math"
	"time"
)

// month peak sources
const (
	sourceMeter = "meter"
	sourceEvcc  = "evcc"
)

const (
	maxMeterPeak    = 50e3             // W, highest month peak accepted from a meter
	meterClockSkew  = 2 * time.Minute  // how far a meter registration may lie in the future
	meterPeakMaxAge = 10 * time.Minute // how long a meter month peak stays valid without a new reading
	peakMismatch    = 50.0             // W difference between meter and own count that is reported
	peakSettle      = 2 * time.Minute  // how long after a slot boundary the meter may still register the slot
)

// Peak is a month peak in W and the start of its slot
type Peak struct {
	Power float64
	Start time.Time
}

// SlotPower returns the average power in W of energy kWh in one slot
func SlotPower(energy float64) float64 {
	return watts(energy, SlotDuration)
}

// MonthPeakFromMeter converts the month peak registered by the meter (OBIS 1-0:1.6.0) into a Peak.
// The meter registers a slot at its end, so the slot starts one slot before the boundary of the
// registration. A peak from an earlier month, or no registration, counts as zero.
func MonthPeakFromMeter(power float64, registered, now time.Time, loc *time.Location) (Peak, error) {
	if math.IsNaN(power) || math.IsInf(power, 0) || power < 0 || power > maxMeterPeak {
		return Peak{}, fmt.Errorf("invalid meter month peak: %v", power)
	}
	if registered.IsZero() {
		if power > 0 {
			return Peak{}, fmt.Errorf("meter month peak without timestamp: %v", power)
		}
		return Peak{}, nil
	}
	if registered.After(now.Add(meterClockSkew)) {
		return Peak{}, fmt.Errorf("meter month peak in the future: %v", registered)
	}

	start := SlotStart(registered).Add(-SlotDuration)
	if start.Before(MonthStart(now, loc)) {
		return Peak{}, nil
	}

	return Peak{Power: power, Start: start}, nil
}

// MonthPeakTracker chooses between the month peak of the meter and evcc's own count.
// The meter is preferred, the own count is the fallback and a check.
type MonthPeakTracker struct {
	evcc, meter Peak
	meterRead   time.Time // last valid meter reading, zero if none
	source      string

	warnedMeter, warnedEvcc float64 // last mismatch reported
}

// MonthPeakState is the result of MonthPeakTracker.Update
type MonthPeakState struct {
	Peak
	Source   string // meter or evcc
	Evcc     Peak
	Switched bool // source differs from the previous update, or first update
	Mismatch bool // meter and own count differ by more than peakMismatch, reported once per pair
}

// SetEvcc sets the month peak from evcc's own count
func (t *MonthPeakTracker) SetEvcc(p Peak) {
	t.evcc = p
}

// SetMeter sets a valid month peak read from the meter at now
func (t *MonthPeakTracker) SetMeter(p Peak, now time.Time) {
	t.meter, t.meterRead = p, now
}

// Update returns the month peak in use at now
func (t *MonthPeakTracker) Update(now time.Time, loc *time.Location) MonthPeakState {
	useMeter := !t.meterRead.IsZero() &&
		now.Sub(t.meterRead) <= meterPeakMaxAge &&
		MonthStart(t.meterRead, loc).Equal(MonthStart(now, loc))

	res := MonthPeakState{Peak: t.evcc, Source: sourceEvcc, Evcc: t.evcc}
	if useMeter {
		res.Peak, res.Source = t.meter, sourceMeter
	}

	res.Switched = t.source != res.Source
	t.source = res.Source

	if !useMeter || now.Sub(SlotStart(now)) < peakSettle {
		return res
	}

	if math.Abs(t.meter.Power-t.evcc.Power) > peakMismatch && (t.meter.Power != t.warnedMeter || t.evcc.Power != t.warnedEvcc) {
		t.warnedMeter, t.warnedEvcc = t.meter.Power, t.evcc.Power
		res.Mismatch = true
	}

	return res
}
