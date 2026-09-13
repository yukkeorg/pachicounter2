package replay

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestLoopMarksRestartAtEachLap(t *testing.T) {
	src, err := New(Options{Path: writeLog(t), Loop: true})
	if err != nil {
		t.Fatalf("信号源を作れません: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := src.Events(ctx)
	if err != nil {
		t.Fatalf("流せません: %v", err)
	}

	// 1 周 2 イベントなので、4 つ読めば 2 周目の頭までの様子が分かる。
	want := []struct {
		baseline bool
		restart  bool
	}{
		{baseline: true, restart: false}, // 1 周目の頭。基準イベント
		{baseline: false, restart: false},
		{baseline: true, restart: true}, // 2 周目の頭。再接続ではなくやり直し
		{baseline: false, restart: false},
	}

	for i, w := range want {
		select {
		case ev := <-events:
			if ev.Baseline != w.baseline || ev.Restart != w.restart {
				t.Errorf("%d 個目: Baseline=%v Restart=%v, 期待は Baseline=%v Restart=%v",
					i, ev.Baseline, ev.Restart, w.baseline, w.restart)
			}
		case <-time.After(time.Second):
			t.Fatalf("%d 個目のイベントが来ません", i)
		}
	}
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
