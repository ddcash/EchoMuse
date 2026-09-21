package sendspin

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

// tone is a 16-bit test signal that exercises real content (sines plus a slow
// ramp) rather than silence.
func tone(n, channels int) []int16 {
	out := make([]int16, n*channels)
	for i := 0; i < n; i++ {
		for c := 0; c < channels; c++ {
			v := 9000*math.Sin(2*math.Pi*440*float64(i)/48000+float64(c)) +
				4000*math.Sin(2*math.Pi*3100*float64(i)/48000) +
				float64((i%97)-48)*20
			out[i*channels+c] = int16(v)
		}
	}
	return out
}

// encodeFLAC returns the stream header ("fLaC" + STREAMINFO) and the frames
// that follow it, which is how the wire splits a FLAC stream: the header once,
// as codec_header, and frames in the audio chunks.
func encodeFLAC(t *testing.T, pcm []int16, channels int) (header, frames []byte) {
	t.Helper()
	var buf bytes.Buffer
	info := &meta.StreamInfo{
		BlockSizeMin: 4096, BlockSizeMax: 4096, SampleRate: 48000,
		NChannels: uint8(channels), BitsPerSample: 16, NSamples: uint64(len(pcm) / channels),
	}
	enc, err := flac.NewEncoder(&buf, info)
	if err != nil {
		t.Fatal(err)
	}
	layout := frame.ChannelsMono
	if channels == 2 {
		layout = frame.ChannelsLR
	}
	const bs = 4096
	total := len(pcm) / channels
	for off := 0; off < total; off += bs {
		n := bs
		if total-off < n {
			n = total - off
		}
		f := &frame.Frame{Header: frame.Header{
			HasFixedBlockSize: true, BlockSize: uint16(n), SampleRate: 48000,
			Channels: layout, BitsPerSample: 16,
		}}
		for c := 0; c < channels; c++ {
			s := make([]int32, n)
			for i := 0; i < n; i++ {
				s[i] = int32(pcm[(off+i)*channels+c])
			}
			f.Subframes = append(f.Subframes, &frame.Subframe{
				SubHeader: frame.SubHeader{Pred: frame.PredVerbatim}, Samples: s, NSamples: n})
		}
		if err := enc.WriteFrame(f); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	const hdr = 4 + 4 + 34
	return buf.Bytes()[:hdr], buf.Bytes()[hdr:]
}

func TestFLACDecodeBitExact(t *testing.T) {
	for _, ch := range []int{1, 2} {
		pcm := tone(10000, ch)
		header, frames := encodeFLAC(t, pcm, ch)
		d, err := newDecoder(Format{Codec: "flac", SampleRate: 48000, Channels: ch, BitDepth: 16, CodecHeader: header})
		if err != nil {
			t.Fatal(err)
		}
		got, err := d.decode(frames)
		if err != nil {
			t.Fatalf("%dch: %v", ch, err)
		}
		if len(got) != len(pcm) {
			t.Fatalf("%dch: decoded %d samples, want %d", ch, len(got), len(pcm))
		}
		for i := range pcm {
			if got[i] != pcm[i] {
				t.Fatalf("%dch: sample %d = %d, want %d", ch, i, got[i], pcm[i])
			}
		}
	}
}

// Chunks are decoded independently, so a decoder that kept state across calls
// would corrupt the audio after a stream/clear.
func TestFLACChunksAreIndependent(t *testing.T) {
	pcm := tone(3*4096, 1)
	header, frames := encodeFLAC(t, pcm, 1)
	d, _ := newDecoder(Format{Codec: "flac", SampleRate: 48000, Channels: 1, BitDepth: 16, CodecHeader: header})
	whole, err := d.decode(frames)
	if err != nil {
		t.Fatal(err)
	}
	// Frames are 4096 samples verbatim, so the last one starts at a known size.
	frameLen := len(frames) / 3
	last, err := d.decode(frames[2*frameLen:])
	if err != nil {
		t.Fatal(err)
	}
	if len(last) != 4096 {
		t.Fatalf("decoded %d samples from the last frame, want 4096", len(last))
	}
	for i := range last {
		if last[i] != whole[2*4096+i] {
			t.Fatalf("independent decode differs at %d", i)
		}
	}
}

func TestFLACRejectsCorruption(t *testing.T) {
	pcm := tone(4096, 1)
	header, frames := encodeFLAC(t, pcm, 1)
	frames[len(frames)/2] ^= 0xFF
	d, _ := newDecoder(Format{Codec: "flac", SampleRate: 48000, Channels: 1, BitDepth: 16, CodecHeader: header})
	if _, err := d.decode(frames); err == nil {
		t.Fatal("a corrupt frame must be reported, not played")
	}
}

func TestPCMDecode(t *testing.T) {
	in := []int16{0, 1, -1, 32767, -32768, 1234}
	b := make([]byte, len(in)*2)
	for i, s := range in {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(s))
	}
	d, _ := newDecoder(Format{Codec: "pcm", SampleRate: 48000, Channels: 2, BitDepth: 16})
	got, err := d.decode(b)
	if err != nil {
		t.Fatal(err)
	}
	for i := range in {
		if got[i] != in[i] {
			t.Fatalf("sample %d = %d", i, got[i])
		}
	}
	if _, err := d.decode(b[:len(b)-1]); err == nil {
		t.Fatal("a partial PCM frame must be rejected")
	}
}

func TestPCM24BitKeepsTopBits(t *testing.T) {
	// 0x123456 packed little-endian is 56 34 12; the top 16 bits are 0x1234.
	d, _ := newDecoder(Format{Codec: "pcm", SampleRate: 48000, Channels: 1, BitDepth: 24})
	got, err := d.decode([]byte{0x56, 0x34, 0x12})
	if err != nil || len(got) != 1 || got[0] != 0x1234 {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestUnsupportedCodec(t *testing.T) {
	if _, err := newDecoder(Format{Codec: "opus", Channels: 2, SampleRate: 48000, BitDepth: 16}); err == nil {
		t.Fatal("opus is not built in and must be refused")
	}
}

func TestToMono(t *testing.T) {
	in := []int16{100, 300, -50, 50}
	for sel, want := range map[string][]int16{"left": {100, -50}, "right": {300, 50}, "mono": {200, 0}} {
		got := toMono(in, 2, sel)
		if got[0] != want[0] || got[1] != want[1] {
			t.Errorf("%s: %v want %v", sel, got, want)
		}
	}
	if got := toMono([]int16{5, 6}, 1, "left"); got[0] != 5 || got[1] != 6 {
		t.Errorf("mono passthrough: %v", got)
	}
}
