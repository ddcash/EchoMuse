package sendspin

import (
	"math"
	"math/rand"
	"testing"
)

const (
	testRate   = 48000
	testPeriod = 2048 // one ALSA period, the unit Pull is called in
)

// The stream carries a ramp: sample value == frame index, so any output sample
// says exactly which frame of the source it is, and therefore what server time
// it belongs to. That turns "is it in sync" into arithmetic instead of a
// judgement about a waveform.
const rampWrap = 30000

func frameTs(base int64, frame int) int64 { return base + int64(frame)*1_000_000/testRate }

func pushRamp(p *Player, base int64, fromFrame, frames, chunk int) {
	for f := fromFrame; f < fromFrame+frames; f += chunk {
		n := chunk
		if fromFrame+frames-f < n {
			n = fromFrame + frames - f
		}
		pcm := make([]int16, n)
		for i := range pcm {
			pcm[i] = int16((f + i) % rampWrap)
		}
		p.Push(frameTs(base, f), pcm)
	}
}

type simResult struct {
	maxErrUs   float64 // worst |sample time error| after settling
	discont    int     // jumps in the ramp that are not a single-frame correction
	audioSpans int
	frames     int
	stats      PlayerStats
}

// simulate drives Pull the way the write loop does: one call per period, with
// wakeup jitter, against a server clock that runs at (1+driftPPM) of local.
func simulate(t *testing.T, p *Player, base int64, startLocal int64, driftPPM float64, offset int64,
	periods int, jitterUs float64, settlePeriods int) simResult {
	t.Helper()
	rng := rand.New(rand.NewSource(1))
	var res simResult
	prev := -1
	inAudio := false
	periodUs := float64(testPeriod) * 1e6 / testRate
	dst := make([]int16, testPeriod)
	for k := 0; k < periods; k++ {
		local := float64(startLocal) + float64(k)*periodUs
		// What the scheduler is told carries the jitter; what is actually due
		// at the speaker does not. Error is judged against the latter.
		truth := int64(local*(1+driftPPM*1e-6)) + offset
		target := truth + int64(rng.NormFloat64()*jitterUs)
		n := p.Pull(dst, target)
		if n > 0 && !inAudio {
			inAudio = true
			res.audioSpans++
		}
		for j := 0; j < n; j++ {
			v := int(dst[j])
			if v == 0 && prev < 0 {
				continue // leading silence inside a lead-in
			}
			if dst[j] == 0 && j > 0 && dst[j-1] == 0 {
				continue
			}
			if prev >= 0 {
				d := v - prev
				if d < 0 {
					d += rampWrap
				}
				// 1 = normal, 0 = duplicated frame, 2 = deleted frame.
				if d > 2 {
					res.discont++
					t.Logf("discontinuity at period %d sample %d: %d -> %d", k, j, prev, v)
				}
			}
			prev = v
			if k >= settlePeriods {
				desired := float64(truth) + float64(j)*1e6/testRate
				actual := float64(frameTs(base, v))
				// The ramp wraps; compare modulo its period.
				wrapUs := float64(rampWrap) * 1e6 / testRate
				e := math.Mod(actual-desired, wrapUs)
				if e > wrapUs/2 {
					e -= wrapUs
				} else if e < -wrapUs/2 {
					e += wrapUs
				}
				if math.Abs(e) > res.maxErrUs {
					res.maxErrUs = math.Abs(e)
				}
			}
			res.frames++
		}
	}
	res.stats = p.Stats()
	return res
}

func TestPlayerStartsInSyncWithLeadIn(t *testing.T) {
	p := NewPlayer(testRate)
	// Audio is due 1.2s after the first pull.
	base := int64(50_000_000)
	pushRamp(p, base, 0, testRate*10, 960)
	r := simulate(t, p, base, base-1_200_000, 0, 0, 200, 0, 60)
	if r.discont != 0 {
		t.Fatalf("%d discontinuities", r.discont)
	}
	if r.maxErrUs > 30 {
		t.Fatalf("steady-state error %.0fµs with no jitter and no drift", r.maxErrUs)
	}
	if r.stats.Snaps != 0 {
		t.Fatalf("lead-in counted as %d snaps", r.stats.Snaps)
	}
}

func TestPlayerTracksClockDriftWithoutGlitches(t *testing.T) {
	for _, ppm := range []float64{-150, -30, 30, 150} {
		p := NewPlayer(testRate)
		base := int64(50_000_000)
		pushRamp(p, base, 0, testRate*30, 960)
		// 20s of playback with 300µs of wakeup jitter.
		r := simulate(t, p, base, base-500_000, ppm, 0, 470, 300, 100)
		if r.discont != 0 {
			t.Errorf("%+.0fppm: %d discontinuities", ppm, r.discont)
		}
		// The spec asks for ±1ms in steady state.
		if r.maxErrUs > 1000 {
			t.Errorf("%+.0fppm: worst error %.0fµs, want under 1000", ppm, r.maxErrUs)
		}
		// 150ppm over 20s is 3ms: about 140 frames. Corrections must be of that
		// order, not a snap storm.
		if r.stats.Snaps > 0 {
			t.Errorf("%+.0fppm: %d snaps in steady drift", ppm, r.stats.Snaps)
		}
	}
}

func TestPlayerCorrectionStaysUnderSpeedCap(t *testing.T) {
	// Even with a large standing error the correction per period is capped at
	// 0.5% of the period (spec: effective speed within +/-0.5%).
	p := NewPlayer(testRate)
	base := int64(50_000_000)
	pushRamp(p, base, 0, testRate*10, 960)
	// Start 3ms off: too small to snap, big enough to need a lot of correcting.
	dst := make([]int16, testPeriod)
	periodUs := float64(testPeriod) * 1e6 / testRate
	maxPerPull := 0
	for k := 0; k < 40; k++ {
		before := p.Stats()
		p.Pull(dst, base+3000+int64(float64(k)*periodUs))
		after := p.Stats()
		d := int(after.Dropped-before.Dropped) + int(after.Inserted-before.Inserted)
		if d > maxPerPull {
			maxPerPull = d
		}
	}
	if limit := testPeriod / 200; maxPerPull > limit {
		t.Fatalf("corrected %d frames in one period, cap is %d", maxPerPull, limit)
	}
}

func TestPlayerSnapsWhenBadlyLate(t *testing.T) {
	p := NewPlayer(testRate)
	base := int64(50_000_000)
	pushRamp(p, base, 0, testRate*10, 960)
	// First pull is 500ms after the audio's start: skip ahead in one shot.
	dst := make([]int16, testPeriod)
	n := p.Pull(dst, base+500_000)
	if n == 0 {
		t.Fatal("nothing played")
	}
	first := int(dst[0])
	want := testRate / 2 // frame index due at +500ms
	if d := first - want; d < -2 || d > 2 {
		t.Fatalf("first sample is frame %d, want ~%d", first, want)
	}
	if got := p.Stats().LateChunks; got == 0 {
		t.Fatal("chunks entirely in the past should have been discarded")
	}
}

func TestPlayerSilentGapInStream(t *testing.T) {
	// The server can leave a hole in the timeline (paused source): the hole
	// must be silence of the right length, and the audio after it on time.
	p := NewPlayer(testRate)
	base := int64(50_000_000)
	pushRamp(p, base, 0, testRate, 960)
	gap := testRate / 2 // 500ms
	pushRamp(p, base, testRate+gap, testRate, 960)
	dst := make([]int16, testPeriod)
	periodUs := float64(testPeriod) * 1e6 / testRate
	var out []int16
	for k := 0; k < 120; k++ {
		p.Pull(dst, base+int64(float64(k)*periodUs))
		out = append(out, dst...)
	}
	// Frame index testRate+gap must land at output index testRate+gap (within a
	// few samples), and the hole before it must be zeros.
	idx := -1
	for i := testRate + gap - 200; i < len(out); i++ {
		if out[i] != 0 {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("audio after the gap never played")
	}
	if want := testRate + gap; idx < want-4 || idx > want+4 {
		t.Fatalf("audio resumed at output sample %d, want ~%d", idx, want)
	}
}

func TestPlayerUnderrunThenRecovery(t *testing.T) {
	p := NewPlayer(testRate)
	base := int64(50_000_000)
	pushRamp(p, base, 0, testRate/2, 960) // half a second, then the link stalls
	dst := make([]int16, testPeriod)
	periodUs := float64(testPeriod) * 1e6 / testRate
	for k := 0; k < 40; k++ {
		p.Pull(dst, base+int64(float64(k)*periodUs))
	}
	if p.Stats().Underruns == 0 {
		t.Fatal("running out of audio mid-stream should count as an underrun")
	}
	// Audio resumes on the original timeline: the part already past is dropped.
	resume := testRate * 2
	pushRamp(p, base, resume, testRate, 960)
	now := base + int64(float64(60)*periodUs) // 2.56s in
	n := p.Pull(dst, now)
	if n == 0 {
		t.Fatal("no audio after recovery")
	}
	// The frame due at 2.56s, not the stale start of the resumed run (2.0s).
	want := (testRate * 256 / 100) % rampWrap
	if d := int(dst[0]) - want; d < -2 || d > 2 {
		t.Fatalf("resumed at frame %d, want ~%d", dst[0], want)
	}
}

func TestPlayerClearDropsEverything(t *testing.T) {
	p := NewPlayer(testRate)
	pushRamp(p, 1_000_000, 0, testRate, 960)
	if p.Buffered() < 0.9 {
		t.Fatalf("buffered %.2fs", p.Buffered())
	}
	p.Clear()
	dst := make([]int16, testPeriod)
	if n := p.Pull(dst, 1_000_000); n != 0 || p.Buffered() != 0 {
		t.Fatalf("after Clear: n=%d buffered=%.2f", n, p.Buffered())
	}
}

func TestPlayerRefusesToBufferWithoutBound(t *testing.T) {
	p := NewPlayer(testRate)
	pushRamp(p, 1_000_000, 0, testRate*30, 960) // 30s offered, cap is 12s
	if b := p.Buffered(); b > maxBufferedSec+0.1 {
		t.Fatalf("buffered %.1fs, cap is %ds", b, maxBufferedSec)
	}
}

func TestPlayClockLocksAndRelocks(t *testing.T) {
	period := int64(42_666)
	c := NewPlayClock(period, 4*period)
	rng := rand.New(rand.NewSource(2))
	var prev int64
	for k := 0; k < 500; k++ {
		now := int64(k)*period + int64(rng.NormFloat64()*400)
		est := c.Next(now)
		if k > 50 {
			// Jitter on the input must not appear at the output: consecutive
			// estimates stay within a small slew of one period.
			if d := est - prev - period; d < -60 || d > 60 {
				t.Fatalf("estimate stepped by %dµs from the period at k=%d", d, k)
			}
		}
		prev = est
	}
	// A stall of many periods re-locks instead of slewing for seconds.
	est := c.Next(int64(600) * period)
	if want := int64(600)*period + 4*period; est != want {
		t.Fatalf("after a stall the clock reads %d, want %d", est, want)
	}
}
