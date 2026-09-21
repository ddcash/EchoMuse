package config

import "testing"

func boolPtr(b bool) *bool { return &b }

// Sendspin opens a listening port, so it must be off until an operator turns it
// on, and off must be expressible over the wire: a plain bool with omitempty
// would make "disable" indistinguishable from "not sent".
func TestSendspinDefaultsOffAndCanBeToggled(t *testing.T) {
	d := &Device{}
	d.loadDefaults()
	d.initialised = true

	if snap := d.Snapshot(); snap.SendspinEnabled == nil || *snap.SendspinEnabled {
		t.Fatalf("Sendspin must default off, got %v", snap.SendspinEnabled)
	}
	if got := d.Snapshot().SendspinStereoChannel; got != StereoMono {
		t.Fatalf("stereo channel defaults to %q, want mono", got)
	}

	d.Apply(ConfigMessage{SendspinEnabled: boolPtr(true), SendspinStereoChannel: "Left"})
	snap := d.Snapshot()
	if !*snap.SendspinEnabled || snap.SendspinStereoChannel != StereoLeft {
		t.Fatalf("after enabling: %v %q", *snap.SendspinEnabled, snap.SendspinStereoChannel)
	}

	// A partial push that does not mention the keys leaves them alone.
	d.Apply(ConfigMessage{OwwThreshold: 0.6})
	snap = d.Snapshot()
	if !*snap.SendspinEnabled || snap.SendspinStereoChannel != StereoLeft {
		t.Fatalf("an unrelated push changed Sendspin: %v %q", *snap.SendspinEnabled, snap.SendspinStereoChannel)
	}

	d.Apply(ConfigMessage{SendspinEnabled: boolPtr(false)})
	if *d.Snapshot().SendspinEnabled {
		t.Fatal("false was not applied")
	}
}

func TestStereoChannelNormalisation(t *testing.T) {
	for in, want := range map[string]string{
		"left": StereoLeft, " RIGHT ": StereoRight, "mono": StereoMono,
		"": StereoMono, "center": StereoMono, "stereo": StereoMono,
	} {
		if got := normaliseStereoChannel(in); got != want {
			t.Errorf("normaliseStereoChannel(%q) = %q, want %q", in, got, want)
		}
	}
}
