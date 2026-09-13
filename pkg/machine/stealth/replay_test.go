package stealth_test

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/yukkeorg/pachicounter2/api"
	"github.com/yukkeorg/pachicounter2/internal/app"
	"github.com/yukkeorg/pachicounter2/internal/config"
	"github.com/yukkeorg/pachicounter2/internal/source/dummy"
	"github.com/yukkeorg/pachicounter2/internal/store"

	_ "github.com/yukkeorg/pachicounter2/pkg/machine/stealth"
)

// replayFixture は実機で録った生信号ログの切り出し（testdata/）を、コアを再起動した
// ときと同じ経路、つまりログの再集計で読み、その集計結果を返す。
func replayFixture(t *testing.T, name string) api.Snapshot {
	t.Helper()

	f, err := os.Open(filepath.Join("testdata", name+".jsonl"))
	if err != nil {
		t.Fatalf("fixture を開けません: %v", err)
	}
	defer f.Close()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("保存先を開けません: %v", err)
	}
	w, err := st.CreateSession(name)
	if err != nil {
		t.Fatalf("セッションのログを作れません: %v", err)
	}
	var start store.Record
	if err := store.ReadRecords(f, func(rec store.Record) error {
		if rec.Kind == store.KindSessionStart {
			start = rec
		}
		return w.Append(rec)
	}); err != nil {
		t.Fatalf("fixture を読めません: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("セッションのログを閉じられません: %v", err)
	}
	if err := st.SaveSnapshot(store.Pointer{Session: w.ID(), Machine: start.Machine}, api.Snapshot{}); err != nil {
		t.Fatalf("続きのセッションを指せません: %v", err)
	}

	core, err := app.New(app.Options{
		MachineID: start.Machine,
		Wiring:    config.Wiring{Bits: start.Wiring, ActiveLow: start.ActiveLow},
		Tuning:    config.DefaultTuning(),
		Ops:       config.DefaultOps(),
		Source:    dummy.New(nil, false),
		Store:     st,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("コアを組み立てられません: %v", err)
	}
	defer core.Close()

	if core.SessionID() != w.ID() {
		t.Fatalf("fixture を続きとして読めていない（セッション %q）", core.SessionID())
	}
	return core.Snapshot()
}

func TestReplayRealMachineLogs(t *testing.T) {
	// 期待値はコードの出力ではなく、生の信号を用語集と ADR-0004 の数え方で数え直したもの。
	// 各区間の中身は testdata/README.md を参照。
	type counts struct {
		normal, densapo, bonuses, firstHits, chain, current int
		state                                               string
	}
	cases := []struct {
		name string
		want counts
	}{
		// パルスの途中から録り始めた分は数えない。11 回転で初当たり、SR なし、その後 1 回転。
		{name: "single-first-hit", want: counts{normal: 12, bonuses: 1, firstHits: 1, current: 1, state: "通常"}},
		// 19 回転で初当たり、SR 中の 27 回転と 1 回転で 2 回連荘、電サポが終わって連荘数は 0。
		{name: "sr-chain-end", want: counts{normal: 19, densapo: 28, bonuses: 3, firstHits: 1, state: "通常"}},
		// 大当りの途中から録り始めたので、その大当りと初当たりは数えない。SR 85 回転で連荘し、
		// SR を 99 回転完走した後の通常 5 回転まで、大当り間回転数は 0 に戻らない。
		{name: "sr-complete", want: counts{normal: 5, densapo: 184, bonuses: 1, current: 104, state: "通常"}},
		// 配線していない入力のうち、ビット 4〜11 がいちばん激しく揺れ始める時間帯。揺れだけの記録が 225 件あり、
		// 回転の立下りと揺れが同じ記録で起きる箇所も含む。揺れは数えず、回転だけを数える。
		{name: "unwired-noise", want: counts{normal: 4, current: 4, state: "通常"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := replayFixture(t, tc.name)
			c := snap.Counters
			got := counts{
				normal: c.NormalRotations, densapo: c.DensapoRotations, bonuses: c.Bonuses,
				firstHits: c.FirstHits, chain: c.Chain, current: c.CurrentRotations, state: snap.State.Label,
			}
			if got != tc.want {
				t.Errorf("集計結果が実機と違う\n  今: 通常時回転数 %d / 電サポ中回転数 %d / 大当り回数 %d / 初当たり回数 %d / 連荘数 %d / 大当り間回転数 %d / 状態 %s\n期待: 通常時回転数 %d / 電サポ中回転数 %d / 大当り回数 %d / 初当たり回数 %d / 連荘数 %d / 大当り間回転数 %d / 状態 %s",
					got.normal, got.densapo, got.bonuses, got.firstHits, got.chain, got.current, got.state,
					tc.want.normal, tc.want.densapo, tc.want.bonuses, tc.want.firstHits, tc.want.chain, tc.want.current, tc.want.state)
			}
		})
	}
}
