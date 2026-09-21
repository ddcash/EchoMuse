package sendspin

import "math"

// TimeFilter is the two-dimensional Kalman filter the Sendspin spec requires
// for mapping the server's clock onto the local one. It tracks offset and
// drift from NTP-style client/time exchanges.
//
// It is a port of aiosendspin's SendspinTimeFilter (itself a 1:1 port of the
// ESPHome reference), and the constants are the reference's. Do not tune them:
// every player in a group must converge the same way for the group to stay in
// sync, which is the property the spec is protecting.
//
// Not safe for concurrent use. Session owns one and serialises access.
type TimeFilter struct {
	lastUpdate int64
	count      int

	offset      float64
	drift       float64
	offsetCov   float64
	offDriftCov float64
	driftCov    float64

	processVar      float64
	driftProcessVar float64
	forgetVarFactor float64

	cur timeElement
}

type timeElement struct {
	lastUpdate int64
	offset     float64
	drift      float64
	useDrift   bool
}

const (
	// adaptiveForgettingCutoff is the residual, as a multiple of maxError, past
	// which the filter forgets history to recover from an outlier.
	adaptiveForgettingCutoff = 3.0
	// maxErrorScale turns the half round-trip delay into a measurement
	// standard deviation. Below 1 because half the RTT overestimates the noise.
	maxErrorScale = 0.5
	// driftSignificanceSq gates drift compensation on SNR: drift is only
	// applied when drift^2 > threshold^2 * drift covariance.
	driftSignificanceSq = 2.0 * 2.0
)

// NewTimeFilter returns a filter with the reference defaults.
func NewTimeFilter() *TimeFilter {
	f := &TimeFilter{
		processVar:      0,
		driftProcessVar: 1e-11 * 1e-11,
		forgetVarFactor: 2.0 * 2.0,
	}
	f.Reset()
	return f
}

// Reset discards all state.
func (f *TimeFilter) Reset() {
	f.count = 0
	f.lastUpdate = 0
	f.offset = 0
	f.drift = 0
	f.offsetCov = math.Inf(1)
	f.offDriftCov = 0
	f.driftCov = 0
	f.cur = timeElement{}
}

// Update folds one measurement in. measurement is ((T2-T1)+(T3-T4))/2, maxError
// is ((T4-T1)-(T3-T2))/2, timeAdded is the local time (µs) the reply arrived.
func (f *TimeFilter) Update(measurement, maxError, timeAdded int64) {
	if timeAdded <= f.lastUpdate {
		// Non-monotonic: a backwards dt would poison the predict step.
		return
	}
	dt := float64(timeAdded - f.lastUpdate)
	f.lastUpdate = timeAdded

	updateStd := float64(maxError) * maxErrorScale
	measVar := updateStd * updateStd

	if f.count <= 0 {
		f.count++
		f.offset = float64(measurement)
		f.offsetCov = measVar
		f.drift = 0
		f.cur = timeElement{lastUpdate: f.lastUpdate, offset: f.offset, drift: f.drift}
		return
	}

	if f.count == 1 {
		f.count++
		f.drift = (float64(measurement) - f.offset) / dt
		f.offset = float64(measurement)
		f.driftCov = (f.offsetCov + measVar) / (dt * dt)
		f.offsetCov = measVar
		f.cur = timeElement{lastUpdate: f.lastUpdate, offset: f.offset, drift: f.drift}
		return
	}

	// Predict.
	offset := f.offset + f.drift*dt
	dtSq := dt * dt

	newDriftCov := f.driftCov + dt*f.driftProcessVar
	newOffDriftCov := f.offDriftCov + f.driftCov*dt
	newOffsetCov := f.offsetCov + 2*f.offDriftCov*dt + f.driftCov*dtSq + dt*f.processVar

	// Innovation and adaptive forgetting.
	residual := float64(measurement) - offset
	cutoff := float64(maxError) * adaptiveForgettingCutoff

	if f.count < 100 {
		f.count++
	} else if math.Abs(residual) > cutoff {
		newDriftCov *= f.forgetVarFactor
		newOffDriftCov *= f.forgetVarFactor
		newOffsetCov *= f.forgetVarFactor
	}

	// Update. The floor keeps a zero-error loopback from dividing by zero.
	uncertainty := 1.0 / math.Max(newOffsetCov+measVar, 1e-9)
	offsetGain := newOffsetCov * uncertainty
	driftGain := newOffDriftCov * uncertainty

	f.offset = offset + offsetGain*residual
	f.drift += driftGain * residual

	f.driftCov = newDriftCov - driftGain*newOffDriftCov
	f.offDriftCov = newOffDriftCov - driftGain*newOffsetCov
	f.offsetCov = newOffsetCov - offsetGain*newOffsetCov

	useDrift := f.drift*f.drift > driftSignificanceSq*f.driftCov
	f.cur = timeElement{lastUpdate: f.lastUpdate, offset: f.offset, drift: f.drift, useDrift: useDrift}
}

// ToServer converts a local timestamp (µs) to the server's clock.
func (f *TimeFilter) ToServer(clientTime int64) int64 {
	e := f.cur
	drift := 0.0
	if e.useDrift {
		drift = e.drift
	}
	dt := float64(clientTime - e.lastUpdate)
	return clientTime + int64(math.Round(e.offset+drift*dt))
}

// ToClient converts a server timestamp (µs) to the local clock.
func (f *TimeFilter) ToClient(serverTime int64) int64 {
	e := f.cur
	drift := 0.0
	if e.useDrift {
		drift = e.drift
	}
	return int64(math.Round((float64(serverTime) - e.offset + drift*float64(e.lastUpdate)) / (1.0 + drift)))
}

// Synchronized reports whether the filter has converged enough to schedule
// playback: at least two measurements and a finite offset covariance. A player
// must not report available=true before this holds.
func (f *TimeFilter) Synchronized() bool {
	return f.count >= 2 && !math.IsInf(f.offsetCov, 0)
}

// ErrorUs is the offset standard deviation estimate in µs.
func (f *TimeFilter) ErrorUs() int64 {
	if math.IsInf(f.offsetCov, 0) {
		return math.MaxInt64
	}
	return int64(math.Round(math.Sqrt(f.offsetCov)))
}

// Count is the number of measurements processed.
func (f *TimeFilter) Count() int { return f.count }
