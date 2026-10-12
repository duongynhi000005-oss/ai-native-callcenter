// SPDX-License-Identifier: Apache-2.0

package transcribe

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// MaxClipSec is the longest clip sent in one request. The model accepts five
// minutes; this leaves headroom so a cut that lands a little late is still
// inside the limit.
const MaxClipSec = 270

// cutSearchSec is how far back from the limit a cut may move to find a quiet
// moment.
const cutSearchSec = 30

// silencePeak is the sample magnitude at or under which a whole clip is
// treated as silence and not sent: a channel nobody spoke on costs audio
// minutes for nothing.
const silencePeak = 64

// PCM is decoded 16-bit audio, one slice per channel.
type PCM struct {
	Rate     int
	Channels [][]int16
}

var errNotPCM16 = errors.New("not a 16-bit PCM WAV file")

// ParseWAV decodes a canonical RIFF/WAVE file of 16-bit PCM. The data chunk's
// declared length is trusted only as far as the bytes that exist: the switch
// can leave a header unpatched when a recording is cut short.
func ParseWAV(b []byte) (PCM, error) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return PCM{}, errNotPCM16
	}
	var (
		channels, bits, format int
		rate                   int
		haveFmt                bool
	)
	pos := 12
	for pos+8 <= len(b) {
		id := string(b[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(b[pos+4 : pos+8]))
		body := pos + 8
		switch id {
		case "fmt ":
			if body+16 > len(b) {
				return PCM{}, errNotPCM16
			}
			format = int(binary.LittleEndian.Uint16(b[body:]))
			channels = int(binary.LittleEndian.Uint16(b[body+2:]))
			rate = int(binary.LittleEndian.Uint32(b[body+4:]))
			bits = int(binary.LittleEndian.Uint16(b[body+14:]))
			haveFmt = true
		case "data":
			if !haveFmt || format != 1 || bits != 16 || channels < 1 || channels > 2 || rate <= 0 {
				return PCM{}, fmt.Errorf("%w (format %d, %d bits, %d channels)", errNotPCM16, format, bits, channels)
			}
			end := body + size
			if size == 0 || end > len(b) || end < body {
				end = len(b)
			}
			data := b[body:end]
			frames := len(data) / (2 * channels)
			pcm := PCM{Rate: rate, Channels: make([][]int16, channels)}
			for c := range pcm.Channels {
				pcm.Channels[c] = make([]int16, frames)
			}
			for i := 0; i < frames; i++ {
				for c := 0; c < channels; c++ {
					pcm.Channels[c][i] = int16(binary.LittleEndian.Uint16(data[(i*channels+c)*2:]))
				}
			}
			return pcm, nil
		}
		pos = body + size + size%2
	}
	return PCM{}, errNotPCM16
}

// Clip is a stretch of one channel, ready to send.
type Clip struct {
	// OffsetMs is where the clip starts in the recording.
	OffsetMs int
	Samples  []int16
}

// Clips cuts one channel into clips of at most MaxClipSec. A cut prefers the
// quietest 20 ms frame in the last cutSearchSec of the window, so it falls in
// a pause rather than in a word. Clips that are all silence are dropped.
func Clips(samples []int16, rate int) []Clip {
	maxLen := MaxClipSec * rate
	search := cutSearchSec * rate
	frame := rate / 50
	if frame < 1 {
		frame = 1
	}

	var out []Clip
	for start := 0; start < len(samples); {
		end := len(samples)
		if end-start > maxLen {
			end = quietestCut(samples, start+maxLen-search, start+maxLen, frame)
		}
		if !isSilent(samples[start:end]) {
			out = append(out, Clip{OffsetMs: start * 1000 / rate, Samples: samples[start:end]})
		}
		start = end
	}
	return out
}

// quietestCut returns the start of the lowest-energy frame in [from, to); ties
// go to the later one, which keeps clips long.
func quietestCut(s []int16, from, to, frame int) int {
	best, bestEnergy := to, int64(-1)
	for f := from; f+frame <= to; f += frame {
		var e int64
		for _, v := range s[f : f+frame] {
			e += int64(v) * int64(v)
		}
		if bestEnergy < 0 || e <= bestEnergy {
			best, bestEnergy = f, e
		}
	}
	return best
}

func isSilent(s []int16) bool {
	for _, v := range s {
		if v > silencePeak || v < -silencePeak {
			return false
		}
	}
	return true
}

// EncodeWAV writes mono 16-bit PCM as a canonical WAV file.
func EncodeWAV(samples []int16, rate int) []byte {
	dataLen := len(samples) * 2
	b := make([]byte, 44+dataLen)
	copy(b[0:], "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+dataLen))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], uint32(rate))
	binary.LittleEndian.PutUint32(b[28:], uint32(rate*2))
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(dataLen))
	for i, v := range samples {
		binary.LittleEndian.PutUint16(b[44+i*2:], uint16(v))
	}
	return b
}
