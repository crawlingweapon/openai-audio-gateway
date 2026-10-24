package transcode

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// buildTestWAV renders 0.1s of 440Hz sine as 44.1kHz mono PCM16 WAV.
func buildTestWAV(t *testing.T) []byte {
	t.Helper()
	const rate = 44100
	const dur = 0.1
	n := int(rate * dur)
	pcm := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := math.Sin(2 * math.Pi * 440 * float64(i) / rate)
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(v*20000)))
	}
	buf := new(bytes.Buffer)
	// RIFF header
	buf.WriteString("RIFF")
	binary.Write(buf, binary.LittleEndian, uint32(36+len(pcm)))
	buf.WriteString("WAVEfmt ")
	binary.Write(buf, binary.LittleEndian, uint32(16))
	buf.Write([]byte{1, 0}) // PCM
	buf.Write([]byte{1, 0}) // mono
	binary.Write(buf, binary.LittleEndian, uint32(rate))
	binary.Write(buf, binary.LittleEndian, uint32(rate*2)) // byte rate
	buf.Write([]byte{2, 0})                                // block align
	buf.Write([]byte{16, 0})                               // bits
	buf.WriteString("data")
	binary.Write(buf, binary.LittleEndian, uint32(len(pcm)))
	buf.Write(pcm)
	return buf.Bytes()
}
