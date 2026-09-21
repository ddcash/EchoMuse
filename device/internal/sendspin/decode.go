package sendspin

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/mewkiz/flac/frame"
)

// Format is what a stream/start announces for the player role.
type Format struct {
	Codec      string
	SampleRate int
	Channels   int
	BitDepth   int
	// CodecHeader is the decoded (standard base64) codec_header, if any.
	CodecHeader []byte
}

// decoder turns one binary audio chunk into interleaved S16 samples.
//
// The spec guarantees a chunk holds whole codec units (whole PCM frames, whole
// FLAC frames), so decoding is stateless per chunk and a lost or discarded
// chunk cannot desynchronise the ones after it. That is what lets stream/clear
// and late-chunk drops be plain buffer operations.
type decoder interface {
	decode(chunk []byte) ([]int16, error)
}

// newDecoder builds a decoder for f, or fails for a codec we did not offer.
//
// Opus is deliberately absent. It needs libopus through cgo, which the NDK
// build cannot link, and the spec lets a player list flac or pcm alone —
// servers must support both.
func newDecoder(f Format) (decoder, error) {
	if f.Channels < 1 || f.Channels > 2 {
		return nil, fmt.Errorf("unsupported channel count %d", f.Channels)
	}
	switch f.Codec {
	case "pcm":
		return pcmDecoder{f}, nil
	case "flac":
		return flacDecoder{f}, nil
	default:
		return nil, fmt.Errorf("unsupported codec %q", f.Codec)
	}
}

// pcmDecoder reads little-endian signed PCM. 24-bit samples are packed as
// three bytes per the spec's PCM convention; anything wider than 16 bits is
// truncated to its top 16, which is all the speaker path carries.
type pcmDecoder struct{ f Format }

func (d pcmDecoder) decode(chunk []byte) ([]int16, error) {
	bps := d.f.BitDepth / 8
	if bps < 2 || bps > 4 {
		return nil, fmt.Errorf("unsupported pcm bit depth %d", d.f.BitDepth)
	}
	frameBytes := bps * d.f.Channels
	if len(chunk)%frameBytes != 0 {
		return nil, fmt.Errorf("pcm chunk of %d bytes is not a whole number of %d-byte frames", len(chunk), frameBytes)
	}
	out := make([]int16, len(chunk)/bps)
	for i := range out {
		// The sample's most significant two bytes are its last two in LE.
		o := i*bps + bps - 2
		out[i] = int16(binary.LittleEndian.Uint16(chunk[o:]))
	}
	return out, nil
}

// flacDecoder decodes the FLAC frames in a chunk. The chunk carries no
// stream header (that arrives once, as codec_header), just frames, and each
// frame's own header states its parameters.
type flacDecoder struct{ f Format }

func (d flacDecoder) decode(chunk []byte) ([]int16, error) {
	r := bytes.NewReader(chunk)
	var out []int16
	for {
		fr, err := frame.Parse(r)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, fmt.Errorf("flac frame: %w", err)
		}
		n := len(fr.Subframes)
		if n != d.f.Channels {
			return out, fmt.Errorf("flac frame has %d channels, stream announced %d", n, d.f.Channels)
		}
		bps := int(fr.BitsPerSample)
		samples := int(fr.BlockSize)
		start := len(out)
		out = append(out, make([]int16, samples*n)...)
		for c, sf := range fr.Subframes {
			if len(sf.Samples) < samples {
				return out[:start], errors.New("flac subframe shorter than block size")
			}
			for i := 0; i < samples; i++ {
				out[start+i*n+c] = toS16(sf.Samples[i], bps)
			}
		}
	}
}

// toS16 scales a sample of the given bit depth to 16 bits. Narrower samples
// are shifted up rather than left small, wider ones are truncated.
func toS16(s int32, bps int) int16 {
	switch {
	case bps == 16:
		return int16(s)
	case bps < 16:
		return int16(s << uint(16-bps))
	default:
		return int16(s >> uint(bps-16))
	}
}

// toMono folds interleaved samples down to one channel according to sel:
// "left" and "right" pick a channel, anything else averages. A mono input
// passes through.
func toMono(in []int16, channels int, sel string) []int16 {
	if channels == 1 {
		return in
	}
	out := make([]int16, len(in)/2)
	switch sel {
	case "left":
		for i := range out {
			out[i] = in[i*2]
		}
	case "right":
		for i := range out {
			out[i] = in[i*2+1]
		}
	default:
		for i := range out {
			out[i] = int16((int32(in[i*2]) + int32(in[i*2+1])) / 2)
		}
	}
	return out
}
