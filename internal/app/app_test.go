package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/yukkeorg/pachicounter2/api"
	"github.com/yukkeorg/pachicounter2/internal/app"
	"github.com/yukkeorg/pachicounter2/internal/config"
	"github.com/yukkeorg/pachicounter2/internal/source/dummy"
	"github.com/yukkeorg/pachicounter2/internal/store"
	"github.com/yukkeorg/pachicounter2/pkg/machine"
	"github.com/yukkeorg/pachicounter2/pkg/signal"

	_ "github.com/yukkeorg/pachicounter2/pkg/machine/stealth"
)

const (
	bitStart   = 0
	bitBonus   = 1
	bitDensapo = 2
)

// ports は「出ている信号」からポートの生値を作る。配線はアクティブローなので、
// 信号が出ているビットは 0 になる。
func ports(activeBits ...int) signal.Ports {
	var p uint16 = 0xffff
	for _, bit := range activeBits {
		p &= ^(uint16(1) << uint(bit))
	}
	return signal.Ports(p)
}

func testWiring() config.Wiring {
	return config.Wiring{
		Bits: map[signal.Role]int{
			signal.RoleStart:   bitStart,
			signal.RoleBonus:   bitBonus,
			signal.RoleDensapo: bitDensapo,
		},
		ActiveLow: true,
	}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// scenario は通常時 4 回転 → 初当たり → 大当り終了 → SR 中 2 回転 という流れ。
func scenario() []dummy.Step {
	steps := []dummy.Step{
		{Ports: ports()},
	}
	for i := 0; i < 4; i++ {
		steps = append(steps,
			dummy.Step{After: 900 * time.Millisecond, Ports: ports(bitStart)},
			dummy.Step{After: 40 * time.Millisecond, Ports: ports()},
		)
	}
	steps = append(steps,
		dummy.Step{After: time.Second, Ports: ports(bitBonus, bitDensapo)},
		dummy.Step{After: 25 * time.Second, Ports: ports(bitDensapo)},
	)
	for i := 0; i < 2; i++ {
		steps = append(steps,
			dummy.Step{After: 900 * time.Millisecond, Ports: ports(bitStart, bitDensapo)},
			dummy.Step{After: 40 * time.Millisecond, Ports: ports(bitDensapo)},
		)
	}
	return steps
}

// waitFor はスナップショットが条件を満たすまで待つ。
func waitFor(t *testing.T, core *app.App, want func(api.Snapshot) bool) api.Snapshot {
	t.Helper()

	updates, cancel := core.Subscribe()
	defer cancel()

	if snap := core.Snapshot(); want(snap) {
		return snap
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case snap, ok := <-updates:
			if !ok {
				t.Fatal("購読が閉じられました")
			}
			if want(snap) {
				return snap
			}
		case <-deadline:
			t.Fatalf("条件を満たすスナップショットが来ませんでした（最後の状態: %+v）", core.Snapshot())
			return api.Snapshot{}
		}
	}
}

func TestPipelineAggregatesAndPersists(t *testing.T) {
	dir := t.TempDir()

	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("保存先を開けません: %v", err)
	}

	opts := app.Options{
		MachineID: "stealth",
		Wiring:    testWiring(),
		Tuning:    config.DefaultTuning(),
		Ops:       config.Ops{MaxSecPerRotation: 40},
		Source:    dummy.New(scenario(), false),
		Store:     st,
		Logger:    quietLogger(),
	}

	core, err := app.New(opts)
	if err != nil {
		t.Fatalf("コアを組み立てられません: %v", err)
	}

	ctx, stop := context.WithCancel(context.Background())
	go func() {
		if err := core.Run(ctx); err != nil {
			t.Errorf("コアが異常終了しました: %v", err)
		}
	}()

	snap := waitFor(t, core, func(s api.Snapshot) bool {
		return s.Counters.DensapoRotations == 2
	})

	if snap.Counters.NormalRotations != 4 {
		t.Errorf("通常時回転数 = %d, 期待は 4", snap.Counters.NormalRotations)
	}
	if snap.Counters.FirstHits != 1 {
		t.Errorf("初当たり回数 = %d, 期待は 1", snap.Counters.FirstHits)
	}
	if snap.Counters.Bonuses != 1 {
		t.Errorf("大当り回数 = %d, 期待は 1", snap.Counters.Bonuses)
	}
	if snap.Derived.FirstHitRate != 4 {
		t.Errorf("初当たり確率の分母 = %v, 期待は 4", snap.Derived.FirstHitRate)
	}
	if snap.State.Label != "STEALTH RUSH" {
		t.Errorf("状態のラベル = %q, 期待は STEALTH RUSH", snap.State.Label)
	}
	// 賞球信号を配線していないので、獲得玉数は推定である。それが分かる形で
	// 出ていなければ、数字の意味を読み手が誤る。
	if snap.Derived.BallsGainedMeasured {
		t.Error("賞球信号が無いのに獲得玉数が実測扱いになっている")
	}

	sessionID := core.SessionID()

	stop()
	if err := core.Close(); err != nil {
		t.Fatalf("セッションを閉じられません: %v", err)
	}

	// 生信号ログが残っていること。
	var records int
	if err := st.ReadSession(sessionID, func(store.Record) error {
		records++
		return nil
	}); err != nil {
		t.Fatalf("生信号ログを読めません: %v", err)
	}
	if records < len(scenario()) {
		t.Errorf("記録されたレコード数 = %d, 信号の数 %d より少ない", records, len(scenario()))
	}
}

func TestResumeRebuildsFromLog(t *testing.T) {
	dir := t.TempDir()

	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("保存先を開けません: %v", err)
	}

	newOpts := func() app.Options {
		return app.Options{
			MachineID: "stealth",
			Wiring:    testWiring(),
			Tuning:    config.DefaultTuning(),
			Ops:       config.Ops{MaxSecPerRotation: 40},
			Store:     st,
			Logger:    quietLogger(),
		}
	}

	// 1 回目。シナリオを流して落とす。
	first := newOpts()
	first.Source = dummy.New(scenario(), false)

	core, err := app.New(first)
	if err != nil {
		t.Fatalf("コアを組み立てられません: %v", err)
	}

	ctx, stop := context.WithCancel(context.Background())
	go func() { _ = core.Run(ctx) }()

	before := waitFor(t, core, func(s api.Snapshot) bool {
		return s.Counters.DensapoRotations == 2
	})
	sessionID := core.SessionID()

	stop()
	if err := core.Close(); err != nil {
		t.Fatalf("セッションを閉じられません: %v", err)
	}

	// 2 回目。信号を 1 つも流さずに立ち上げ、ログの再集計だけで
	// 同じ数字に戻ることを見る。スナップショットを読むだけでは機種プラグインの
	// 内部状態が戻らないので、状態のラベルも一緒に確かめる。
	second := newOpts()
	second.Source = dummy.New([]dummy.Step{}, false)

	resumed, err := app.New(second)
	if err != nil {
		t.Fatalf("セッションを続けられません: %v", err)
	}
	defer resumed.Close()

	if got := resumed.SessionID(); got != sessionID {
		t.Errorf("セッション識別子 = %q, 期待は %q", got, sessionID)
	}

	after := resumed.Snapshot()
	if !reflect.DeepEqual(after.Counters, before.Counters) {
		t.Errorf("復帰後のカウンタ = %+v, 期待は %+v", after.Counters, before.Counters)
	}
	if after.State != before.State {
		t.Errorf("復帰後の状態 = %+v, 期待は %+v（プラグインの内部状態がログから戻っていない）",
			after.State, before.State)
	}

	// 止まっていた間の空白が、ログだけを見ても分かること。
	var last store.Record
	if err := st.ReadSession(sessionID, func(rec store.Record) error {
		last = rec
		return nil
	}); err != nil {
		t.Fatalf("生信号ログを読めません: %v", err)
	}
	if last.Kind != store.KindResume {
		t.Errorf("再開した直後の最後の記録 = %q, 期待は %q", last.Kind, store.KindResume)
	}
}

func TestNewSessionResetsCounters(t *testing.T) {
	dir := t.TempDir()

	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("保存先を開けません: %v", err)
	}

	core, err := app.New(app.Options{
		MachineID: "stealth",
		Wiring:    testWiring(),
		Tuning:    config.DefaultTuning(),
		Ops:       config.Ops{MaxSecPerRotation: 40},
		Source:    dummy.New(scenario(), false),
		Store:     st,
		Logger:    quietLogger(),
	})
	if err != nil {
		t.Fatalf("コアを組み立てられません: %v", err)
	}
	defer core.Close()

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() { _ = core.Run(ctx) }()

	// シナリオを最後まで流し切ってから新しいセッションを始める。途中で始めると
	// 残りの信号が新しいセッションに流れ込む（それ自体は正しい挙動である）。
	waitFor(t, core, func(s api.Snapshot) bool { return s.Counters.DensapoRotations == 2 })

	oldSession := core.SessionID()
	if err := core.NewSession("テスト"); err != nil {
		t.Fatalf("新しいセッションを始められません: %v", err)
	}

	if core.SessionID() == oldSession {
		t.Error("セッション識別子が変わっていない")
	}

	// 始めた理由がログの 1 行目に残ること。API は覚書を受け取るのに、コアが捨てていた。
	start, err := st.SessionStart(core.SessionID())
	if err != nil {
		t.Fatalf("新しいセッションの 1 行目を読めません: %v", err)
	}
	if start.Note != "テスト" {
		t.Errorf("session_start の覚書 = %q, 期待は テスト", start.Note)
	}

	snap := core.Snapshot()
	if !reflect.DeepEqual(snap.Counters, machine.Counters{}) {
		t.Errorf("新しいセッションのカウンタ = %+v, 期待はすべて 0", snap.Counters)
	}
	if snap.State.Label != "通常" {
		t.Errorf("新しいセッションの状態 = %q, 期待は通常", snap.State.Label)
	}
}

func TestCorrectIsRecordedInLog(t *testing.T) {
	dir := t.TempDir()

	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("保存先を開けません: %v", err)
	}

	core, err := app.New(app.Options{
		MachineID: "stealth",
		Wiring:    testWiring(),
		Tuning:    config.DefaultTuning(),
		Ops:       config.Ops{MaxSecPerRotation: 40},
		Source:    dummy.New(scenario(), false),
		Store:     st,
		Logger:    quietLogger(),
	})
	if err != nil {
		t.Fatalf("コアを組み立てられません: %v", err)
	}

	ctx, stop := context.WithCancel(context.Background())
	go func() { _ = core.Run(ctx) }()

	waitFor(t, core, func(s api.Snapshot) bool { return s.Counters.NormalRotations == 4 })

	if err := core.Correct(api.CorrectRequest{
		Counter: "normal_rotations",
		Delta:   1,
		Note:    "取りこぼし",
	}); err != nil {
		t.Fatalf("補正できません: %v", err)
	}

	snap := waitFor(t, core, func(s api.Snapshot) bool { return s.Counters.NormalRotations == 5 })
	if snap.Counters.NormalRotations != 5 {
		t.Fatalf("補正後の通常時回転数 = %d, 期待は 5", snap.Counters.NormalRotations)
	}

	sessionID := core.SessionID()
	stop()
	if err := core.Close(); err != nil {
		t.Fatalf("セッションを閉じられません: %v", err)
	}

	// 補正が生信号ログに残っていること。ここが残っていないと、再集計で
	// 補正が消える。詳細は ADR-0008 を参照。
	var found *store.Record
	if err := st.ReadSession(sessionID, func(rec store.Record) error {
		if rec.Kind == store.KindCorrect {
			copied := rec
			found = &copied
		}
		return nil
	}); err != nil {
		t.Fatalf("生信号ログを読めません: %v", err)
	}

	if found == nil {
		t.Fatal("補正が生信号ログに記録されていない")
	}
	if found.Counter != "normal_rotations" || found.Delta != 1 || found.Note != "取りこぼし" {
		t.Errorf("記録された補正 = %+v", *found)
	}
}

func TestResumeWithDifferentWiringStartsNewSession(t *testing.T) {
	cases := []struct {
		name   string
		wiring func() config.Wiring
	}{
		{
			name: "ビット位置が違う",
			wiring: func() config.Wiring {
				w := testWiring()
				w.Bits[signal.RoleStart] = bitBonus
				w.Bits[signal.RoleBonus] = bitStart
				return w
			},
		},
		{
			name: "アクティブローが違う",
			wiring: func() config.Wiring {
				w := testWiring()
				w.ActiveLow = false
				return w
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatalf("保存先を開けません: %v", err)
			}

			newOpts := func(w config.Wiring, src signal.Source) app.Options {
				return app.Options{
					MachineID: "stealth",
					Wiring:    w,
					Tuning:    config.DefaultTuning(),
					Ops:       config.Ops{MaxSecPerRotation: 40},
					Source:    src,
					Store:     st,
					Logger:    quietLogger(),
				}
			}

			// 1 回目。記録どおりの配線でシナリオを流して落とす。
			core, err := app.New(newOpts(testWiring(), dummy.New(scenario(), false)))
			if err != nil {
				t.Fatalf("コアを組み立てられません: %v", err)
			}

			ctx, stop := context.WithCancel(context.Background())
			go func() { _ = core.Run(ctx) }()

			waitFor(t, core, func(s api.Snapshot) bool { return s.Counters.DensapoRotations == 2 })
			sessionID := core.SessionID()

			stop()
			if err := core.Close(); err != nil {
				t.Fatalf("セッションを閉じられません: %v", err)
			}

			// 2 回目。配線だけ変えて立ち上げる。前のログを今の配線で読み直すと
			// 数字が別物になるので、続けずに新しいセッションになっていなければならない。
			resumed, err := app.New(newOpts(tc.wiring(), dummy.New([]dummy.Step{}, false)))
			if err != nil {
				t.Fatalf("コアを組み立てられません: %v", err)
			}
			defer resumed.Close()

			if got := resumed.SessionID(); got == sessionID {
				t.Errorf("配線が違うのに前のセッション %q を続けている", got)
			}
			if got := resumed.Snapshot().Counters; !reflect.DeepEqual(got, machine.Counters{}) {
				t.Errorf("新しいセッションのカウンタ = %+v, 期待はすべて 0", got)
			}
		})
	}
}

func TestReplayDoesNotPersist(t *testing.T) {
	core, err := app.New(app.Options{
		MachineID: "stealth",
		Wiring:    testWiring(),
		Tuning:    config.DefaultTuning(),
		Ops:       config.Ops{MaxSecPerRotation: 40},
		Source:    dummy.New(scenario(), false),
		Replay:    true,
		Logger:    quietLogger(),
	})
	if err != nil {
		t.Fatalf("コアを組み立てられません: %v", err)
	}
	defer core.Close()

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() { _ = core.Run(ctx) }()

	// 集計そのものは実機のときと同じに動くこと。
	snap := waitFor(t, core, func(s api.Snapshot) bool { return s.Counters.DensapoRotations == 2 })
	if snap.Counters.NormalRotations != 4 {
		t.Errorf("通常時回転数 = %d, 期待は 4", snap.Counters.NormalRotations)
	}

	if got := core.SessionID(); got != "" {
		t.Errorf("再生なのにセッション %q に属している", got)
	}

	// 補正も新しいセッションも記録を変える操作なので、何も書かない再生では断る。
	if err := core.Correct(api.CorrectRequest{Counter: "normal_rotations", Delta: 1}); !errors.Is(err, app.ErrReplay) {
		t.Errorf("再生中の補正のエラー = %v, 期待は ErrReplay", err)
	}
	if err := core.NewSession(""); !errors.Is(err, app.ErrReplay) {
		t.Errorf("再生中の新しいセッションのエラー = %v, 期待は ErrReplay", err)
	}
}

func TestReplayRejectsStore(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("保存先を開けません: %v", err)
	}

	// 再生に保存先を渡せてしまうと、書かないはずの再生が書ける形が残る。
	if _, err := app.New(app.Options{
		MachineID: "stealth",
		Wiring:    testWiring(),
		Source:    dummy.New(nil, false),
		Store:     st,
		Replay:    true,
		Logger:    quietLogger(),
	}); err == nil {
		t.Fatal("再生なのに保存先を受け取った")
	}
}

func TestReconnectIsRecordedOnlyFromSecondBaseline(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("保存先を開けません: %v", err)
	}

	// dummy は Connected() が常に true を返す。開始時の接続状態で再接続を判断して
	// いたときは、最初の基準イベントを再接続と取り違えてログに disconnect を書いていた。
	steps := []dummy.Step{
		{Ports: ports()},
		{After: 100 * time.Millisecond, Ports: ports(bitStart)},
		{After: 40 * time.Millisecond, Ports: ports()},
		{After: time.Second, Ports: ports(), Baseline: true}, // 抜き差しして繋がり直した
		{After: 100 * time.Millisecond, Ports: ports(bitStart)},
	}

	core, err := app.New(app.Options{
		MachineID: "stealth",
		Wiring:    testWiring(),
		Tuning:    config.DefaultTuning(),
		Ops:       config.Ops{MaxSecPerRotation: 40},
		Source:    dummy.New(steps, false),
		Store:     st,
		Logger:    quietLogger(),
	})
	if err != nil {
		t.Fatalf("コアを組み立てられません: %v", err)
	}

	ctx, stop := context.WithCancel(context.Background())
	go func() { _ = core.Run(ctx) }()

	waitFor(t, core, func(s api.Snapshot) bool { return s.Counters.NormalRotations == 2 })
	sessionID := core.SessionID()

	stop()
	if err := core.Close(); err != nil {
		t.Fatalf("セッションを閉じられません: %v", err)
	}

	var kinds []store.Kind
	if err := st.ReadSession(sessionID, func(rec store.Record) error {
		kinds = append(kinds, rec.Kind)
		return nil
	}); err != nil {
		t.Fatalf("生信号ログを読めません: %v", err)
	}

	want := []store.Kind{
		store.KindSessionStart,
		store.KindBaseline,
		store.KindSignal,
		store.KindSignal,
		store.KindDisconnect, // 2 回目の基準イベントの前にだけ付く
		store.KindBaseline,
		store.KindSignal,
	}
	if !reflect.DeepEqual(kinds, want) {
		t.Errorf("記録の並び = %v, 期待は %v", kinds, want)
	}
}

func TestRestartResetsCountersOnlyWhenReplaying(t *testing.T) {
	// 通常時 2 回転のあと、記録の先頭に戻って初当たりを引く。やり直しが効けば
	// 通常時回転数は 0 に戻り、効かなければ 2 のまま大当りが足される。
	steps := func() []dummy.Step {
		return []dummy.Step{
			{Ports: ports()},
			{After: 100 * time.Millisecond, Ports: ports(bitStart)},
			{After: 40 * time.Millisecond, Ports: ports()},
			{After: 100 * time.Millisecond, Ports: ports(bitStart)},
			{After: 40 * time.Millisecond, Ports: ports()},
			{After: 100 * time.Millisecond, Ports: ports(), Restart: true},
			{After: 100 * time.Millisecond, Ports: ports(bitBonus, bitDensapo)},
		}
	}

	cases := []struct {
		name       string
		replay     bool
		wantNormal int
	}{
		{name: "再生ではやり直す", replay: true, wantNormal: 0},
		// 記録しているセッションで黙ってやり直すと、ログを再集計しても同じ数字にならない。
		{name: "記録中はやり直さない", replay: false, wantNormal: 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := app.Options{
				MachineID: "stealth",
				Wiring:    testWiring(),
				Tuning:    config.DefaultTuning(),
				Ops:       config.Ops{MaxSecPerRotation: 40},
				Source:    dummy.New(steps(), false),
				Replay:    tc.replay,
				Logger:    quietLogger(),
			}
			if !tc.replay {
				st, err := store.Open(t.TempDir())
				if err != nil {
					t.Fatalf("保存先を開けません: %v", err)
				}
				opts.Store = st
			}

			core, err := app.New(opts)
			if err != nil {
				t.Fatalf("コアを組み立てられません: %v", err)
			}
			defer core.Close()

			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			go func() { _ = core.Run(ctx) }()

			snap := waitFor(t, core, func(s api.Snapshot) bool { return s.Counters.Bonuses == 1 })
			if snap.Counters.NormalRotations != tc.wantNormal {
				t.Errorf("通常時回転数 = %d, 期待は %d", snap.Counters.NormalRotations, tc.wantNormal)
			}
		})
	}
}
