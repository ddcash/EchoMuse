package sendspin

import (
	"math"
	"sync"
)

// Player is the jitter buffer and scheduler for one Sendspin stream. The
// session pushes decoded chunks stamped in the server's clock; the speaker's
// write loop pulls one period at a time and says when that period will reach
// the speaker, in local time. Player answers with the audio that belongs at
// exactly that instant.
//
// It pulls rather than pushes because the ALSA write loop is the only thing on
// the device that knows when a sample becomes audible. A push model has to
// guess how deep the music queue is, and a 43ms period is already longer than
// the ±1ms the spec asks of a player.
//
// Correction follows the spec's suggested strategy: whole-frame deletion and
// duplication, bit-exact everywhere else, capped at ±0.5% of the period, with a
// one-shot snap for errors too large to correct smoothly.
type Player struct {
	mu   sync.Mutex
	rate int

	chunks  []chunk
	headOff int // samples already consumed from chunks[0]
	// buffered is the total samples queued, kept for the depth cap.
	buffered int

	// haveExpect is set once audio has been handed out, so an empty queue
	// afterwards reads as an underrun and not as a stream that never started.
	haveExpect bool

	// errAvg smooths the measured scheduling error (µs). One pull's error is
	// dominated by write-loop wakeup jitter, and correcting each one would
	// itself be audible as warble.
	errAvg  float64
	errInit bool

	stats PlayerStats
}

type chunk struct {
	ts  int64 // server µs of the first sample
	pcm []int16
}

// PlayerStats are counters for diagnostics; the sync error is the number that
// says whether the whole exercise is working.
type PlayerStats struct {
	Pulls      uint64
	Underruns  uint64 // pulls that found the queue empty mid-stream
	Snaps      uint64 // one-shot resynchronisations
	Dropped    uint64 // frames deleted by soft correction
	Inserted   uint64 // frames duplicated by soft correction
	LateChunks uint64 // chunks discarded because their time had passed
	ErrUs      int64  // smoothed scheduling error, µs (+ = audio early)
}

const (
	// deadbandUs: below this the chunk is played untouched (spec: ~100µs).
	deadbandUs = 100.0
	// snapUs: above this a smooth correction would take too long and would
	// exceed the speed cap, so snap in one shot. The spec's own floor is ±1ms
	// steady state; this is the point past which we stop pretending.
	snapUs = 5000.0
	// errAlpha weights a new measurement against the running average.
	errAlpha = 0.25
	// maxBufferedSec caps queued audio. The server paces to buffer_capacity, so
	// this is a backstop against a server that does not, not a working limit.
	maxBufferedSec = 12
)

// NewPlayer returns a Player for a stream at rate Hz (mono S16).
func NewPlayer(rate int) *Player {
	return &Player{rate: rate}
}

// Push queues one decoded chunk whose first sample is due at serverTs.
func (p *Player) Push(serverTs int64, pcm []int16) {
	if len(pcm) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.buffered+len(pcm) > maxBufferedSec*p.rate {
		return
	}
	p.chunks = append(p.chunks, chunk{ts: serverTs, pcm: pcm})
	p.buffered += len(pcm)
}

// Clear discards everything queued: stream/clear, stream/end, or a format
// change that cannot be spliced.
func (p *Player) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.chunks = nil
	p.headOff = 0
	p.buffered = 0
	p.haveExpect = false
	p.errInit = false
	p.errAvg = 0
}

// Buffered returns the queued audio in seconds.
func (p *Player) Buffered() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return float64(p.buffered) / float64(p.rate)
}

// Stats returns a snapshot of the counters.
func (p *Player) Stats() PlayerStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.stats
	s.ErrUs = int64(p.errAvg)
	return s
}

func (p *Player) usFor(frames int) float64 { return float64(frames) * 1e6 / float64(p.rate) }
func (p *Player) framesFor(us float64) int { return int(math.Round(us * float64(p.rate) / 1e6)) }

// headTs is the server timestamp of the next unread sample.
func (p *Player) headTs() int64 {
	c := p.chunks[0]
	return c.ts + int64(p.usFor(p.headOff))
}

// Pull fills dst with the audio that should reach the speaker at targetServerUs
// (the server-clock time of dst[0], already adjusted for output delay). It
// returns the number of leading samples that belong to the stream, including any
// silence inside it; the rest of dst is zeroed, so a caller can treat a short
// return as "nothing follows".
//
// Returns 0 when there is nothing to play, which is the normal idle answer, and
// also while a stream's first sample is still in the future.
func (p *Player) Pull(dst []int16, targetServerUs int64) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(dst)
	if len(p.chunks) == 0 {
		if p.haveExpect {
			p.stats.Underruns++
		}
		return 0
	}
	p.stats.Pulls++

	// Drop whole chunks that are late by more than the snap threshold. Counted
	// apart from correction because it means the link, not the clock, is at
	// fault. A chunk that is merely a little behind is left to the smooth
	// correction below: dropping the last few frames of a chunk because the
	// clock estimate jittered past its end would be a several-frame jump.
	for len(p.chunks) > 0 {
		c := p.chunks[0]
		if c.ts+int64(p.usFor(len(c.pcm)))+int64(snapUs) > targetServerUs {
			break
		}
		p.buffered -= len(c.pcm) - p.headOff
		p.chunks = p.chunks[1:]
		p.headOff = 0
		p.stats.LateChunks++
		p.errInit = false
	}
	if len(p.chunks) == 0 {
		return 0
	}

	errUs := float64(p.headTs() - targetServerUs) // + = audio is ahead of the speaker
	if !p.errInit {
		p.errAvg = errUs
		p.errInit = true
	} else {
		p.errAvg += errAlpha * (errUs - p.errAvg)
	}

	lead := 0 // frames of silence before the audio
	edits := 0
	dup := false // edits duplicate frames (audio early) rather than delete them
	switch {
	case math.Abs(errUs) > snapUs:
		// One-shot resync: silence up to the audio's start, or skip the excess.
		if errUs > 0 {
			lead = p.framesFor(errUs)
		} else {
			p.discard(p.framesFor(-errUs))
		}
		p.errAvg = 0
		p.errInit = false
		// Lead-in silence before a stream's first sample is due is not a
		// resynchronisation, and counting it would make the stat useless.
		if p.haveExpect {
			p.stats.Snaps++
		}
	case math.Abs(p.errAvg) > deadbandUs:
		// One frame is 21µs at 48kHz. The spec caps the correction at 0.5% of
		// the period; within that, take as many single-frame edits as the error
		// asks for, spread across the period so each is a one-sample step.
		edits = p.framesFor(math.Abs(p.errAvg))
		if maxN := max(1, len(dst)/200); edits > maxN {
			edits = maxN
		}
		if edits < 1 {
			edits = 1
		}
		if p.errAvg > 0 {
			dup = true
			p.errAvg -= p.usFor(edits)
			p.stats.Inserted += uint64(edits)
		} else {
			p.errAvg += p.usFor(edits)
			p.stats.Dropped += uint64(edits)
		}
	}
	if lead > len(dst) {
		lead = len(dst)
	}
	room := len(dst) - lead

	// Source frames this pull consumes: a duplicate consumes one fewer, a
	// deletion one more.
	need := room
	if dup {
		need -= edits
	} else {
		need += edits
	}
	raw := p.collect(need)
	if len(raw) == 0 {
		return 0
	}

	// Compose the period, applying the edits at evenly spaced positions.
	out := lead
	j := 0
	spacing := room
	if edits > 0 {
		spacing = room / (edits + 1)
	}
	editsLeft, sinceEdit := edits, 0
	for out < len(dst) && j < len(raw) {
		if editsLeft > 0 && sinceEdit >= spacing {
			sinceEdit = 0
			editsLeft--
			if dup {
				dst[out] = raw[j] // repeat this frame: j stays put for one output
				out++
				sinceEdit++
				if out >= len(dst) {
					break
				}
			} else {
				j++ // delete this frame
				if j >= len(raw) {
					break
				}
			}
		}
		dst[out] = raw[j]
		out++
		j++
		sinceEdit++
	}
	if len(raw) < need {
		// The queue ran dry inside the period: what we have plays, the rest is
		// silence, and the next pull will count the underrun.
		return out
	}
	return out
}

// collect takes up to n frames from the head of the queue as one contiguous
// run. A hole in the server's timeline between two chunks becomes silence, and
// an overlap is dropped, so the run is correct against the timeline and not
// merely against arrival order. The first chunk is not checked: how it relates
// to "now" is the caller's alignment decision, and judging it here as well
// would count the same gap twice.
func (p *Player) collect(n int) []int16 {
	raw := make([]int16, 0, n)
	var prevEnd int64
	first := true
	for len(raw) < n && len(p.chunks) > 0 {
		c := &p.chunks[0]
		if !first && p.headOff == 0 {
			gapUs := float64(c.ts - prevEnd)
			if gapUs > deadbandUs {
				g := min(p.framesFor(gapUs), n-len(raw))
				raw = append(raw, make([]int16, g)...)
				prevEnd += int64(p.usFor(g))
				if prevEnd < c.ts-int64(deadbandUs) {
					break // the hole outlasts this period; resume next pull
				}
				continue
			}
			if gapUs < -deadbandUs {
				p.discard(p.framesFor(-gapUs))
				if len(p.chunks) == 0 {
					break
				}
				c = &p.chunks[0]
			}
		}
		avail := c.pcm[p.headOff:]
		take := min(n-len(raw), len(avail))
		raw = append(raw, avail[:take]...)
		p.headOff += take
		p.buffered -= take
		prevEnd = c.ts + int64(p.usFor(p.headOff))
		p.haveExpect = true
		first = false
		if p.headOff >= len(c.pcm) {
			p.chunks = p.chunks[1:]
			p.headOff = 0
		}
	}
	return raw
}

// discard deletes n frames from the head of the queue.
func (p *Player) discard(n int) {
	for n > 0 && len(p.chunks) > 0 {
		c := p.chunks[0]
		avail := len(c.pcm) - p.headOff
		if n < avail {
			p.headOff += n
			p.buffered -= n
			return
		}
		p.buffered -= avail
		n -= avail
		p.chunks = p.chunks[1:]
		p.headOff = 0
	}
}

// PlayClock estimates when the period about to be written will become audible.
//
// The speaker's ALSA ring holds four periods and Pump blocks until one has been
// consumed, so a period submitted now reaches the DAC four periods from now in
// steady state. The estimate is phase-locked rather than re-measured every
// call: Pump returns on a scheduler wakeup with a millisecond or so of jitter,
// and feeding that straight into Pull would turn it into audible correction.
type PlayClock struct {
	periodUs  int64
	latencyUs int64
	est       int64
	valid     bool
}

// NewPlayClock returns a clock for periods of periodUs µs whose audio becomes
// audible latencyUs after the call that submits it.
func NewPlayClock(periodUs, latencyUs int64) *PlayClock {
	return &PlayClock{periodUs: periodUs, latencyUs: latencyUs}
}

// Next takes the local time (µs) at which the next period is about to be
// written and returns when that period will be audible.
func (c *PlayClock) Next(nowUs int64) int64 {
	meas := nowUs + c.latencyUs
	if !c.valid {
		c.est, c.valid = meas, true
		return c.est
	}
	pred := c.est + c.periodUs
	// A residual beyond half a period is a stall or a paused loop, not jitter:
	// re-lock rather than slew toward it.
	if d := meas - pred; d > c.periodUs/2 || d < -c.periodUs/2 {
		c.est = meas
		return c.est
	}
	c.est = pred + (meas-pred)/32
	return c.est
}

// Reset forces the next call to re-lock.
func (c *PlayClock) Reset() { c.valid = false }
