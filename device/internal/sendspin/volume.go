package sendspin

import "math"

// Sendspin volume is PERCEIVED loudness, 0-100, and the spec converts it to a
// linear amplitude as (volume/100)^1.5. The Echo's codec volume is already a dB
// scale (0.5dB steps, unity at index 127), so the conversion that follows the
// spec is a straight line in dB:
//
//	gain_dB = 20*log10((v/100)^1.5) = 30*log10(v/100)
//	index   = unity + 2*gain_dB     = unity + 60*log10(v/100)
//
// Volume 50 lands 9dB below unity, 10 lands 30dB below. A linear v-to-index
// map would put "50%" at -32dB, which is what people call "almost off".
const dbPerVolumeDecade = 30.0 // gain_dB = 30*log10(v/100)

// VolumeToLevel maps a Sendspin volume (0-100) to the codec index, where unity
// is the index for 0dB and steps are 0.5dB. 0 means silence and maps to 0.
func VolumeToLevel(v, unity int) int {
	if v <= 0 {
		return 0
	}
	if v > 100 {
		v = 100
	}
	idx := float64(unity) + 2*dbPerVolumeDecade*math.Log10(float64(v)/100)
	return clampInt(int(math.Round(idx)), 0, unity)
}

// LevelToVolume is the inverse, for reporting the device's volume to the server.
// The codec range is wider than the perceptual one at the bottom, so anything
// below the volume that maps to index 0 reads as 0.
func LevelToVolume(level, unity int) int {
	if level <= 0 {
		return 0
	}
	if level >= unity {
		return 100
	}
	v := 100 * math.Pow(10, float64(level-unity)/(2*dbPerVolumeDecade))
	return clampInt(int(math.Round(v)), 0, 100)
}
