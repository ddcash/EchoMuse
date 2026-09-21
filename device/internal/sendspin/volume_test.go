package sendspin

import (
	"math"
	"testing"
)

const testUnity = 127

func TestVolumeMapFollowsTheSpecCurve(t *testing.T) {
	// The spec: amplitude = (v/100)^1.5. Check the codec gain the index gives
	// against that, to within one 0.5dB step.
	for _, v := range []int{100, 90, 75, 50, 25, 10, 5} {
		idx := VolumeToLevel(v, testUnity)
		gotDb := float64(idx-testUnity) * 0.5
		wantDb := 20 * math.Log10(math.Pow(float64(v)/100, 1.5))
		if math.Abs(gotDb-wantDb) > 0.5 {
			t.Errorf("v=%d: index %d is %.1fdB, spec says %.1fdB", v, idx, gotDb, wantDb)
		}
	}
	if got := VolumeToLevel(100, testUnity); got != testUnity {
		t.Errorf("100 must be unity, got %d", got)
	}
	if got := VolumeToLevel(0, testUnity); got != 0 {
		t.Errorf("0 must be silence, got %d", got)
	}
}

func TestVolumeMapIsMonotonicAndBounded(t *testing.T) {
	prev := -1
	for v := 0; v <= 100; v++ {
		idx := VolumeToLevel(v, testUnity)
		if idx < prev {
			t.Fatalf("index fell from %d to %d at v=%d", prev, idx, v)
		}
		if idx < 0 || idx > testUnity {
			t.Fatalf("index %d out of range at v=%d", idx, v)
		}
		prev = idx
	}
	for _, v := range []int{-5, 101, 1000} {
		if idx := VolumeToLevel(v, testUnity); idx < 0 || idx > testUnity {
			t.Errorf("v=%d gave out-of-range index %d", v, idx)
		}
	}
}

// What the device reports must, when the server sets it back, land on the same
// index, so a slider that echoes our own state does not walk the volume.
func TestVolumeRoundTripIsStable(t *testing.T) {
	for level := 20; level <= testUnity; level++ {
		v := LevelToVolume(level, testUnity)
		back := VolumeToLevel(v, testUnity)
		// Coarse at the bottom (one percent step is several codec steps there),
		// so the tolerance is the width of one volume step at that level.
		tol := 1
		if level < 70 {
			tol = 8
		}
		if d := back - level; d < -tol || d > tol {
			t.Fatalf("level %d -> volume %d -> level %d", level, v, back)
		}
	}
	if LevelToVolume(0, testUnity) != 0 || LevelToVolume(testUnity, testUnity) != 100 || LevelToVolume(200, testUnity) != 100 {
		t.Fatal("endpoints wrong")
	}
}
