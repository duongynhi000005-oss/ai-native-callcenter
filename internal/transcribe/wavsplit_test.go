// SPDX-License-Identifier: Apache-2.0

package transcribe

import (
	"encoding/binary"
	"testing"
)

// stereoWAV interleaves two channels into a canonical file.
func stereoWAV(left, right []int16, rate int) []byte {
	n := len(left)
	b := make([]byte, 44+n*4)
	copy(b[0:], "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+n*4))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 2)
	binary.LittleEndian.PutUint32(b[24:], uint32(rate))
	binary.LittleEndian.PutUint32(b[28:], uint32(rate*4))
	binary.LittleEndian.PutUint16(b[32:], 4)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(n*4))
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint16(b[44+i*4:], uint16(left[i]))
		binary.LittleEndian.PutUint16(b[46+i*4:], uint16(right[i]))
	}
	return b
}

func TestParseWAVSplitsTheChannels(t *testing.T) {
	t.Parallel()
	pcm, err := ParseWAV(stereoWAV([]int16{1, 2, 3}, []int16{-1, -2, -3}, 8000))
	if err != nil {
		t.Fatal(err)
	}
	if pcm.Rate != 8000 || len(pcm.Channels) != 2 {
		t.Fatalf("got rate %d, %d channels", pcm.Rate, len(pcm.Channels))
	}
	if pcm.Channels[0][2] != 3 || pcm.Channels[1][2] != -3 {
		t.Errorf("channels = %v", pcm.Channels)
	}
}

// The switch can leave the header unpatched when a recording is cut short; the
// bytes that exist are the truth.
func TestParseWAVTrustsTheBytesOverAnUnpatchedHeader(t *testing.T) {
	t.Parallel()
	b := stereoWAV([]int16{1, 2}, []int16{3, 4}, 8000)
	binary.LittleEndian.PutUint32(b[40:], 0xFFFFFFFF)
	pcm, err := ParseWAV(b)
	if err != nil || len(pcm.Channels[0]) != 2 {
		t.Fatalf("err %v, %+v", err, pcm)
	}
	binary.LittleEndian.PutUint32(b[40:], 0)
	if pcm, err = ParseWAV(b); err != nil || len(pcm.Channels[0]) != 2 {
		t.Fatalf("zero length: err %v, %+v", err, pcm)
	}
}

func TestParseWAVRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	if _, err := ParseWAV([]byte("not a wav file at all")); err == nil {
		t.Error("garbage was accepted")
	}
	b := stereoWAV([]int16{1}, []int16{1}, 8000)
	binary.LittleEndian.PutUint16(b[34:], 8)
	if _, err := ParseWAV(b); err == nil {
		t.Error("8-bit audio was accepted")
	}
}

func TestEncodeWAVRoundTrips(t *testing.T) {
	t.Parallel()
	in := []int16{5, -7, 300}
	pcm, err := ParseWAV(EncodeWAV(in, 16000))
	if err != nil || pcm.Rate != 16000 || len(pcm.Channels) != 1 {
		t.Fatalf("err %v %+v", err, pcm)
	}
	for i, v := range in {
		if pcm.Channels[0][i] != v {
			t.Errorf("sample %d = %d, want %d", i, pcm.Channels[0][i], v)
		}
	}
}

// loud returns n samples of audible signal.
func loud(n int) []int16 {
	s := make([]int16, n)
	for i := range s {
		s[i] = 1000
	}
	return s
}

func TestAShortChannelIsOneClipAtOffsetZero(t *testing.T) {
	t.Parallel()
	clips := Clips(loud(8000*60), 8000)
	if len(clips) != 1 || clips[0].OffsetMs != 0 || len(clips[0].Samples) != 8000*60 {
		t.Fatalf("clips = %d", len(clips))
	}
}

func TestALongChannelIsCutInAPauseBeforeTheLimit(t *testing.T) {
	t.Parallel()
	const rate = 8000
	// 10 minutes of speech with a one-second pause at 4:10 (250 s), which
	// is inside the 30 s the cut may move back over.
	s := loud(rate * 600)
	pauseAt := rate * 250
	for i := pauseAt; i < pauseAt+rate; i++ {
		s[i] = 0
	}
	clips := Clips(s, rate)
	if len(clips) < 3 {
		t.Fatalf("a 10 minute channel made %d clips", len(clips))
	}
	for i, c := range clips {
		if len(c.Samples) > MaxClipSec*rate {
			t.Errorf("clip %d is %d s, over the limit", i, len(c.Samples)/rate)
		}
	}
	first := len(clips[0].Samples)
	if first < pauseAt || first >= pauseAt+rate {
		t.Errorf("first cut at %d s, want inside the pause at 250-251 s", first/rate)
	}
	if clips[1].OffsetMs != first*1000/rate {
		t.Errorf("second clip offset %d ms, want %d", clips[1].OffsetMs, first*1000/rate)
	}
	var total int
	for _, c := range clips {
		total += len(c.Samples)
	}
	if total != len(s) {
		t.Errorf("clips hold %d samples, the channel has %d: the cuts lost audio", total, len(s))
	}
}

func TestASilentClipIsNotSent(t *testing.T) {
	t.Parallel()
	if got := Clips(make([]int16, 8000*20), 8000); len(got) != 0 {
		t.Errorf("a silent channel made %d clips", len(got))
	}
	// Silence in the middle of speech is still part of a clip.
	s := loud(8000 * 10)
	for i := 8000 * 3; i < 8000*6; i++ {
		s[i] = 0
	}
	if got := Clips(s, 8000); len(got) != 1 || len(got[0].Samples) != len(s) {
		t.Errorf("a pause split the clip: %d clips", len(got))
	}
}
