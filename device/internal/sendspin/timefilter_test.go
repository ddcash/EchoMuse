package sendspin

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

// The vectors come from aiosendspin's SendspinTimeFilter, itself a port of the
// ESPHome reference, driven with a drifting server clock seen through a noisy
// link that includes a congestion burst. Every player in a group has to
// converge the same way, so the check is against the real implementation and
// not against our reading of it.
func TestTimeFilterMatchesReference(t *testing.T) {
	raw, err := os.ReadFile("testdata/timefilter_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var steps []struct {
		Measurement  int64 `json:"measurement"`
		MaxError     int64 `json:"max_error"`
		TimeAdded    int64 `json:"time_added"`
		Synchronized bool  `json:"synchronized"`
		Count        int   `json:"count"`
		Error        int64 `json:"error"`
		ToServer     int64 `json:"to_server"`
		ToClient     int64 `json:"to_client"`
		Probe        int64 `json:"probe"`
	}
	if err := json.Unmarshal(raw, &steps); err != nil {
		t.Fatal(err)
	}
	f := NewTimeFilter()
	for i, s := range steps {
		f.Update(s.Measurement, s.MaxError, s.TimeAdded)
		if f.Synchronized() != s.Synchronized || f.Count() != s.Count {
			t.Fatalf("step %d: synchronized/count = %v/%d, want %v/%d", i, f.Synchronized(), f.Count(), s.Synchronized, s.Count)
		}
		if !s.Synchronized {
			continue
		}
		// Same arithmetic in float64, so agreement should be exact; allow 1µs
		// for rounding at the .5 boundary.
		if d := f.ToServer(s.Probe) - s.ToServer; d < -1 || d > 1 {
			t.Fatalf("step %d: ToServer off by %dµs", i, d)
		}
		if d := f.ToClient(s.Probe+5_000_000) - s.ToClient; d < -1 || d > 1 {
			t.Fatalf("step %d: ToClient off by %dµs", i, d)
		}
		if d := f.ErrorUs() - s.Error; d < -1 || d > 1 {
			t.Fatalf("step %d: error estimate off by %dµs", i, d)
		}
	}
}

func TestTimeFilterRoundTrip(t *testing.T) {
	f := NewTimeFilter()
	for i := int64(1); i <= 20; i++ {
		f.Update(7_000_000, 500, i*1_000_000)
	}
	for _, c := range []int64{25_000_000, 40_000_000, 1_000_000_000} {
		if got := f.ToClient(f.ToServer(c)); math.Abs(float64(got-c)) > 1 {
			t.Fatalf("round trip of %d gave %d", c, got)
		}
	}
	if got := f.ToServer(25_000_000) - 25_000_000; got < 6_999_000 || got > 7_001_000 {
		t.Fatalf("offset = %d, want ~7000000", got)
	}
}

func TestTimeFilterIgnoresBackwardsTime(t *testing.T) {
	f := NewTimeFilter()
	f.Update(1000, 100, 5_000_000)
	f.Update(1000, 100, 6_000_000)
	n := f.Count()
	f.Update(9_999_999, 100, 6_000_000) // same timestamp: would give dt=0
	f.Update(9_999_999, 100, 1_000_000) // backwards
	if f.Count() != n {
		t.Fatalf("non-monotonic updates were folded in")
	}
}
