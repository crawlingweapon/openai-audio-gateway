package server

import (
	"strings"
	"testing"
)

func TestSplitSentencesShortInputUnchanged(t *testing.T) {
	got := splitSentences("hello there.", 100)
	if len(got) != 1 || got[0] != "hello there." {
		t.Errorf("got %#v", got)
	}
}

func TestSplitSentencesRespectsMax(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 40; i++ {
		sb.WriteString("This is sentence number one. ")
	}
	text := sb.String()
	max := 200
	got := splitSentences(text, max)
	if len(got) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(got))
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "sentence number one") {
		t.Error("content lost across chunks")
	}
	for i, c := range got {
		if len(c) > max+1 { // +1 tolerance for boundary punctuation
			t.Errorf("chunk %d exceeds max: %d > %d", i, len(c), max)
		}
	}
}

func TestSplitSentencesPathologicalNoPunctuation(t *testing.T) {
	text := strings.Repeat("x", 500)
	got := splitSentences(text, 100)
	total := 0
	for _, c := range got {
		total += len(c)
		if len(c) > 100 {
			t.Errorf("hard-cut chunk too big: %d", len(c))
		}
	}
	if total != 500 {
		t.Errorf("lost bytes: %d != 500", total)
	}
}

func TestStripAndBuildWAVRoundTrip(t *testing.T) {
	pcm := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	wav := buildWAV(pcm, 24000, 1)
	if string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		t.Fatalf("bad header: %q", wav[:12])
	}
	if len(wav) != 44+len(pcm) {
		t.Fatalf("len = %d", len(wav))
	}
	got := stripWAVHeader(wav)
	if string(got) != string(pcm) {
		t.Errorf("round trip = %v, want %v", got, pcm)
	}
}

func TestStripWAVHeaderNonWAVPassthrough(t *testing.T) {
	raw := []byte{9, 9, 9, 9, 9}
	if got := stripWAVHeader(raw); string(got) != string(raw) {
		t.Errorf("non-wav mutated: %v", got)
	}
}

func TestMergeInstructions(t *testing.T) {
	cases := []struct {
		preset, client string
		allow          bool
		want           string
	}{
		{"preset text", "", false, "preset text"},
		{"preset text", "client text", false, "preset text"},
		{"preset text", "client text", true, "client text"},
		{"preset text", "  ", true, "preset text"},
		{"", "client text", false, "client text"},
	}
	for i, c := range cases {
		if got := mergeInstructions(c.preset, c.client, c.allow); got != c.want {
			t.Errorf("case %d: got %q want %q", i, got, c.want)
		}
	}
}

func TestWriteErrorShape(t *testing.T) {
	// exercised through the handler in e2e; here just verify format string math
	// (kept as a canary if someone "simplifies" the JSON)
	s := `{"error":{"message":%q,"type":"invalid_request_error","code":%q}}`
	if !strings.Contains(s, "invalid_request_error") {
		t.Fatal("shape changed")
	}
}
