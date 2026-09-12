package counter_test

import (
	"testing"
	"time"

	"github.com/yukkeorg/pachicounter/internal/config"
	"github.com/yukkeorg/pachicounter/internal/counter"
	"github.com/yukkeorg/pachicounter/pkg/machine"
	_ "github.com/yukkeorg/pachicounter/pkg/machine/stealth"
	"github.com/yukkeorg/pachicounter/pkg/signal"
)

// ビット位置。回路図どおりの割り当て。
const (
	bitStart       = 0
	bitBonus       = 1
	bitDensapo     = 2
	bitPayoutBonus = 3
)

// ports は「出ている信号」からポートの生値を作る。
//
// 配線はアクティブローなので、信号が出ているビットは 0 になる。テストの側で
// この反転を書いておくことで、engine が反転を正しく扱っているかも一緒に見る。
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
			signal.RoleStart:       bitStart,
			signal.RoleBonus:       bitBonus,
			signal.RoleDensapo:     bitDensapo,
			signal.RolePayoutBonus: bitPayoutBonus,
		},
		ActiveLow: true,
	}
}

// step は信号列の 1 手。
type step struct {
	after time.Duration
	ports signal.Ports
}

// newEngine はデバウンスを切った状態の集計エンジンを作る。デバウンスを見る
// テストだけが個別に有効にする。
func newEngine(t *testing.T, ops config.Ops) *counter.Engine {
	t.Helper()

	plugin, err := machine.New("stealth", "")
	if err != nil {
		t.Fatalf("機種プラグインを作れません: %v", err)
	}

	e, err := counter.New(plugin, testWiring(), config.DefaultTuning(), ops)
	if err != nil {
		t.Fatalf("集計エンジンを作れません: %v", err)
	}
	return e
}

// feed は信号列を流す。1 手目は基準イベントとして扱う。
func feed(e *counter.Engine, steps []step) {
	var at time.Duration
	for i, s := range steps {
		at += s.after
		ev := signal.Event{At: at, Wall: time.Unix(0, 0).Add(at), Ports: s.ports}

		if i == 0 {
			e.Baseline(ev)
			continue
		}
		e.Apply(ev)
	}
}

// rotate は 1 回転ぶんのパルス（立上りと立下り）を信号列に足す。
func rotate(steps []step, held ...int) []step {
	up := append([]int{bitStart}, held...)
	return append(steps,
		step{after: 900 * time.Millisecond, ports: ports(up...)},
		step{after: 40 * time.Millisecond, ports: ports(held...)},
	)
}

func TestBaselineDoesNotCount(t *testing.T) {
	// 大当り中にコアを起動しても、大当り回数は増えない。旧実装はここで
	// 1 増えていた。詳細は ADR-0009 を参照。
	e := newEngine(t, config.Ops{})

	feed(e, []step{
		{ports: ports(bitBonus, bitDensapo)},
	})

	got := e.Counters()
	if got.Bonuses != 0 {
		t.Errorf("大当り回数 = %d, 期待は 0（基準イベントはエッジではない）", got.Bonuses)
	}
	if got.FirstHits != 0 {
		t.Errorf("初当たり回数 = %d, 期待は 0", got.FirstHits)
	}

	// ただし状態表示は大当り中に合っていてほしい。
	if kind := e.Snapshot().State.Kind; kind != machine.StateBonus {
		t.Errorf("状態 = %q, 期待は %q", kind, machine.StateBonus)
	}
}

func TestNormalRotationsCount(t *testing.T) {
	e := newEngine(t, config.Ops{})

	steps := []step{{ports: ports()}}
	for i := 0; i < 5; i++ {
		steps = rotate(steps)
	}
	feed(e, steps)

	got := e.Counters()
	if got.NormalRotations != 5 {
		t.Errorf("通常時回転数 = %d, 期待は 5", got.NormalRotations)
	}
	if got.DensapoRotations != 0 {
		t.Errorf("電サポ中回転数 = %d, 期待は 0", got.DensapoRotations)
	}
	if got.CurrentRotations != 5 {
		t.Errorf("現在の回転数 = %d, 期待は 5", got.CurrentRotations)
	}
}

func TestFirstHitAndChain(t *testing.T) {
	// 通常時に 10 回転して初当たり、SR 中に 2 連荘してから転落する流れ。
	//
	// 電サポ信号は大当り中も出るため、初当たりでは大当り信号と同時に立つ。
	// 初当たりをそこで 1 回だけ数えられているかを見る。
	e := newEngine(t, config.Ops{})

	steps := []step{{ports: ports()}}
	for i := 0; i < 10; i++ {
		steps = rotate(steps)
	}

	// 初当たり。大当り信号と電サポ信号が同時に立つ。
	steps = append(steps, step{after: time.Second, ports: ports(bitBonus, bitDensapo, bitPayoutBonus)})
	// 大当り終了。電サポは続く（SR 突入）。
	steps = append(steps, step{after: 30 * time.Second, ports: ports(bitDensapo)})

	// SR 中に 3 回転してから当たる、を 2 回。
	for chain := 0; chain < 2; chain++ {
		for i := 0; i < 3; i++ {
			steps = rotate(steps, bitDensapo)
		}
		steps = append(steps, step{after: time.Second, ports: ports(bitBonus, bitDensapo, bitPayoutBonus)})
		steps = append(steps, step{after: 30 * time.Second, ports: ports(bitDensapo)})
	}

	// 転落。電サポが落ちる。
	steps = append(steps, step{after: 2 * time.Second, ports: ports()})

	feed(e, steps)
	got := e.Counters()

	if got.FirstHits != 1 {
		t.Errorf("初当たり回数 = %d, 期待は 1（電サポの立上りは 1 回だけ）", got.FirstHits)
	}
	if got.Bonuses != 3 {
		t.Errorf("大当り回数 = %d, 期待は 3", got.Bonuses)
	}
	if got.NormalRotations != 10 {
		t.Errorf("通常時回転数 = %d, 期待は 10（SR 中の回転は分母に入らない）", got.NormalRotations)
	}
	if got.DensapoRotations != 6 {
		t.Errorf("電サポ中回転数 = %d, 期待は 6", got.DensapoRotations)
	}
	if got.Chain != 0 {
		t.Errorf("連荘数 = %d, 期待は 0（転落で戻る）", got.Chain)
	}

	// 初当たり確率は通常時回転数 ÷ 初当たり回数。SR 中の 6 回転は入らない。
	if rate := e.Snapshot().Derived.FirstHitRate; rate != 10 {
		t.Errorf("初当たり確率の分母 = %v, 期待は 10", rate)
	}
}

func TestChainCountsWithinRun(t *testing.T) {
	e := newEngine(t, config.Ops{})

	steps := []step{
		{ports: ports()},
		// 初当たり。
		{after: time.Second, ports: ports(bitBonus, bitDensapo)},
		{after: 30 * time.Second, ports: ports(bitDensapo)},
		// SR 中の 2 回目。
		{after: 5 * time.Second, ports: ports(bitBonus, bitDensapo)},
	}
	feed(e, steps)

	got := e.Counters()
	if got.Chain != 2 {
		t.Errorf("連荘数 = %d, 期待は 2（初当たりが 1 回目）", got.Chain)
	}
	if len(got.BonusHistory) != 2 {
		t.Fatalf("連荘履歴の件数 = %d, 期待は 2", len(got.BonusHistory))
	}
	if got.BonusHistory[0].Chain != 1 || got.BonusHistory[1].Chain != 2 {
		t.Errorf("連荘履歴 = %+v, 期待は 1, 2 の順", got.BonusHistory)
	}
}

func TestDebounceIgnoresChatter(t *testing.T) {
	// 接点のバウンスで 1 回転が 2 回数えられないことを見る。
	ops := config.Ops{Debounce: 8 * time.Millisecond}
	e := newEngine(t, ops)

	steps := []step{
		{ports: ports()},
		// 本物の立上り。
		{after: time.Second, ports: ports(bitStart)},
		// 2ms 後に落ちて 2ms 後に立つ、というチャタリング。
		{after: 2 * time.Millisecond, ports: ports()},
		{after: 2 * time.Millisecond, ports: ports(bitStart)},
		{after: 2 * time.Millisecond, ports: ports()},
		{after: 2 * time.Millisecond, ports: ports(bitStart)},
		// 落ち着いてから本物の立下り。
		{after: 40 * time.Millisecond, ports: ports()},
		// 次の回転。
		{after: time.Second, ports: ports(bitStart)},
		{after: 40 * time.Millisecond, ports: ports()},
	}
	feed(e, steps)

	if got := e.Counters().NormalRotations; got != 2 {
		t.Errorf("通常時回転数 = %d, 期待は 2（チャタリングを数えない）", got)
	}
}

func TestDebounceResolvesAfterLockout(t *testing.T) {
	// デバウンスで見送った立下りが、ロックアウトが明けたあとに追いつくことを見る。
	// 信号源は値が変わったときだけイベントを流すので、Tick が無いと取り残される。
	ops := config.Ops{Debounce: 8 * time.Millisecond}
	e := newEngine(t, ops)

	feed(e, []step{
		{ports: ports()},
		{after: time.Second, ports: ports(bitStart)},
		// ロックアウト中に落ちる。ここでは確定しない。
		{after: 3 * time.Millisecond, ports: ports()},
	})

	if got := e.Counters().NormalRotations; got != 1 {
		t.Fatalf("通常時回転数 = %d, 期待は 1", got)
	}
	if !e.Pending() {
		t.Fatal("立下りがデバウンス待ちのはずだが Pending が false")
	}

	// ロックアウトが明けた時刻で Tick すると追いつく。
	e.Tick(time.Second+20*time.Millisecond, time.Unix(0, 0))
	if e.Pending() {
		t.Error("Tick のあとも Pending が true のまま")
	}

	// もう 1 回転できることで、立下りが確定したと分かる。
	feed2 := []step{
		{after: time.Second + 900*time.Millisecond, ports: ports(bitStart)},
	}
	var at = time.Second + 20*time.Millisecond
	for _, s := range feed2 {
		at += s.after
		e.Apply(signal.Event{At: at, Wall: time.Unix(0, 0).Add(at), Ports: s.ports})
	}

	if got := e.Counters().NormalRotations; got != 2 {
		t.Errorf("通常時回転数 = %d, 期待は 2", got)
	}
}

func TestCorrectRejectsUnknownCounter(t *testing.T) {
	e := newEngine(t, config.Ops{})

	if err := e.Correct("voutput", 1); err == nil {
		t.Error("知らないカウンタの補正が通ってしまった")
	}
	if err := e.Correct("current_rotations", -1); err == nil {
		t.Error("負になる補正が通ってしまった")
	}
	if err := e.Correct("normal_rotations", 3); err != nil {
		t.Errorf("正しい補正が通らない: %v", err)
	}
	if got := e.Counters().NormalRotations; got != 3 {
		t.Errorf("補正後の通常時回転数 = %d, 期待は 3", got)
	}
}

func TestRequiredRolesAreChecked(t *testing.T) {
	plugin, err := machine.New("stealth", "")
	if err != nil {
		t.Fatalf("機種プラグインを作れません: %v", err)
	}

	// 電サポを繋いでいない配線では、動き出す前に落ちてほしい。
	wiring := config.Wiring{
		Bits: map[signal.Role]int{
			signal.RoleStart: bitStart,
			signal.RoleBonus: bitBonus,
		},
		ActiveLow: true,
	}

	if _, err := counter.New(plugin, wiring, config.DefaultTuning(), config.Ops{}); err == nil {
		t.Error("必要な信号が無いのにエンジンが作れてしまった")
	}
}
