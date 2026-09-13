package replay

import (
	"os"
	"path/filepath"
	"testing"
)

func writeLog(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "rec.jsonl")
	body := `{"type":"session_start","at_ns":0,"session":"s","machine":"stealth","wiring":{"start":0,"bonus":1,"densapo":2},"active_low":true}
{"type":"baseline","at_ns":0,"ports":15}
{"type":"signal","at_ns":100000000,"ports":14}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("記録を書けません: %v", err)
	}
	return path
}

func TestRecordedReadsSessionStart(t *testing.T) {
	src, err := New(Options{Path: writeLog(t)})
	if err != nil {
		t.Fatalf("信号源を作れません: %v", err)
	}

	rec, err := src.Recorded()
	if err != nil {
		t.Fatalf("記録した設定を読めません: %v", err)
	}
	if rec.Machine != "stealth" {
		t.Errorf("機種 = %q, 期待は stealth", rec.Machine)
	}
	if rec.Wiring.Bits["start"] != 0 || rec.Wiring.Bits["bonus"] != 1 || !rec.Wiring.ActiveLow {
		t.Errorf("配線 = %+v", rec.Wiring)
	}
}
