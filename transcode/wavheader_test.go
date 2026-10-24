package transcode

import (
	"encoding/binary"
	"testing"
)

func TestFixWAVHeaderCanonical(t *testing.T) {
	// canonical header with WRONG placeholder sizes (what ffmpeg streams to a pipe)
	data := make([]byte, 44+1000)
	copy(data[0:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], 0xFFFFFFFF) // unpatchable placeholder
	copy(data[8:12], "WAVE")
	copy(data[12:16], "fmt ")
	binary.LittleEndian.PutUint32(data[16:20], 16)
	data[20], data[21] = 1, 0 // PCM
	data[22], data[23] = 1, 0 // mono
	binary.LittleEndian.PutUint32(data[24:28], 16000)
	copy(data[36:40], "data")
	binary.LittleEndian.PutUint32(data[40:44], 0xFFFFFFFF) // ditto

	fixed := fixWAVHeader(data)
	riff := binary.LittleEndian.Uint32(fixed[4:8])
	ds := binary.LittleEndian.Uint32(fixed[40:44])
	if riff != uint32(len(fixed)-8) {
		t.Errorf("RIFF size = %d, want %d", riff, len(fixed)-8)
	}
	if ds != 1000 {
		t.Errorf("data size = %d, want 1000", ds)
	}
}

func TestFixWAVHeaderWalksChunks(t *testing.T) {
	// non-canonical layout: extra LIST chunk before data
	// RIFF(12) + fmt(8+16) + LIST(8+4) + data(8+100)
	total := 12 + 24 + 12 + 8 + 100
	b := make([]byte, total)
	copy(b[0:4], "RIFF")
	binary.LittleEndian.PutUint32(b[4:8], 1) // wrong
	copy(b[8:12], "WAVE")
	copy(b[12:16], "fmt ")
	binary.LittleEndian.PutUint32(b[16:20], 16)
	copy(b[36:40], "LIST")
	binary.LittleEndian.PutUint32(b[40:44], 4)
	copy(b[48:52], "data")
	binary.LittleEndian.PutUint32(b[52:56], 0xFFFFFFFF) // wrong
	fixed := fixWAVHeader(b)
	if got := binary.LittleEndian.Uint32(fixed[52:56]); got != 100 {
		t.Errorf("walked data size = %d, want 100", got)
	}
	if got := binary.LittleEndian.Uint32(fixed[4:8]); got != uint32(total-8) {
		t.Errorf("walked RIFF size = %d, want %d", got, total-8)
	}
}

func TestFixWAVHeaderNonWAVUntouched(t *testing.T) {
	mp3 := []byte("ID3\x04not a wav at all........")
	if string(fixWAVHeader(mp3)) != string(mp3) {
		t.Error("non-wav mutated")
	}
	short := []byte("RIFF")
	if string(fixWAVHeader(short)) != string(short) {
		t.Error("short input mutated")
	}
}

func TestFixWAVHeaderAlreadyCorrectIdempotent(t *testing.T) {
	wav := buildTestWAV(t)
	before := append([]byte{}, wav...)
	after := fixWAVHeader(wav)
	if string(before) != string(after) {
		t.Error("correct header was modified")
	}
}
