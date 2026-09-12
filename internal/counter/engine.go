// Package counter は信号を集計結果に変える。
//
// ポート値からエッジを起こし、デバウンスをかけ、機種プラグインに解釈させ、
// 玉の増減を計算してスナップショットを組み立てる。
package counter

import (
	"fmt"
	"sort"
	"time"

	"github.com/yukkeorg/pachicounter/api"
	"github.com/yukkeorg/pachicounter/internal/config"
	"github.com/yukkeorg/pachicounter/pkg/machine"
	"github.com/yukkeorg/pachicounter/pkg/signal"
)

// ballsPer250 は貸玉 250 個を 1 単位とする回転率の分母。4 円パチンコの千円分。
const ballsPer250 = 250.0

// Engine は 1 セッション分の集計を受け持つ。
//
// 並行に呼ばれることは想定していない。呼び出し側が 1 つの goroutine から使う。
type Engine struct {
	plugin machine.Plugin
	spec   machine.Spec
	wiring config.Wiring
	tuning config.Tuning
	ops    config.Ops

	counters machine.Counters

	// observed は信号源から最後に受け取ったポート値を役割ごとに解いたもの。
	observed machine.RoleSet

	// accepted はデバウンスを通して確定した状態。
	accepted machine.RoleSet

	// acceptedAt は役割ごとに最後に状態を確定した時刻。デバウンスの起点。
	acceptedAt map[signal.Role]time.Duration

	haveBaseline bool

	// lastAt は最後に処理したイベントの時刻。経過時間の表示に使う。
	lastAt time.Duration

	// lastRotationAt は最後の回転の時刻。回転間の時間から玉の増減を出す。
	lastRotationAt time.Duration
	haveRotation   bool
	secPerRotation float64

	ballsGained float64
	ballsSpent  float64
}

// New は集計エンジンを作る。プラグインが要求する役割が配線されていなければ
// エラーを返す。動き出してから「その信号が無い」と気づくより、起動時に落ちる方がよい。
func New(p machine.Plugin, w config.Wiring, t config.Tuning, o config.Ops) (*Engine, error) {
	spec := p.Spec()

	var missing []string
	for _, role := range spec.RequiredRoles {
		if !w.Has(role) {
			missing = append(missing, string(role))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("機種 %s が必要とする信号が配線されていません: %v（現在の配線: %s）",
			spec.ID, missing, w)
	}

	e := &Engine{
		plugin:     p,
		spec:       spec,
		wiring:     w,
		tuning:     t,
		ops:        o,
		observed:   machine.RoleSet{},
		accepted:   machine.RoleSet{},
		acceptedAt: map[signal.Role]time.Duration{},
	}
	p.Reset()
	return e, nil
}

// Reset は集計を初期状態に戻す。新しいセッションを始めるときに呼ぶ。
func (e *Engine) Reset() {
	e.counters = machine.Counters{}
	e.observed = machine.RoleSet{}
	e.accepted = machine.RoleSet{}
	e.acceptedAt = map[signal.Role]time.Duration{}
	e.haveBaseline = false
	e.lastAt = 0
	e.lastRotationAt = 0
	e.haveRotation = false
	e.secPerRotation = 0
	e.ballsGained = 0
	e.ballsSpent = 0
	e.plugin.Reset()
}

// Elapsed は最後に処理したイベントまでの経過時間を返す。
func (e *Engine) Elapsed() time.Duration { return e.lastAt }

// PayoutWired は賞球信号が配線されているかを返す。獲得玉数が実測かどうかがこれで決まる。
func (e *Engine) PayoutWired() bool { return e.wiring.Has(signal.RolePayout) }

// Baseline は基準となるポート値を取り込む。エッジは起こさない。
//
// 起動時や再接続時に立っているビットを立上りとして数えると、大当り中にコアを
// 起動しただけで大当り回数が増える。詳細は
// docs/adr/0009-signal-baseline-and-device-loss.md を参照。
func (e *Engine) Baseline(ev signal.Event) {
	e.lastAt = ev.At
	e.observed = e.decode(ev.Ports)

	e.accepted = machine.RoleSet{}
	for role, on := range e.observed {
		e.accepted[role] = on
		e.acceptedAt[role] = ev.At
	}
	e.haveBaseline = true

	e.plugin.Baseline(e.accepted)
}

// Apply はポート値の変化を取り込み、確定したエッジを機種プラグインに渡す。
// 確定したエッジが 1 つ以上あれば true を返す。
func (e *Engine) Apply(ev signal.Event) bool {
	if !e.haveBaseline {
		e.Baseline(ev)
		return false
	}

	e.lastAt = ev.At
	e.observed = e.decode(ev.Ports)
	return e.resolve(ev.At, ev.Wall)
}

// Tick はデバウンス待ちのまま止まっている状態を確定させる。
//
// 信号源は値が変わったときだけイベントを流すので、デバウンスで見送ったエッジを
// 後から拾い直す機会が無い。呼び出し側がデバウンス間隔で定期的に呼ぶことで、
// ロックアウトが明けた時点の実際の値へ追いつく。
func (e *Engine) Tick(at time.Duration, wall time.Time) bool {
	if !e.haveBaseline {
		return false
	}
	if at > e.lastAt {
		e.lastAt = at
	}
	return e.resolve(at, wall)
}

// Pending はデバウンス待ちの状態があるかを返す。
func (e *Engine) Pending() bool {
	for _, role := range e.wiring.Roles() {
		if e.observed[role] != e.accepted[role] {
			return true
		}
	}
	return false
}

// resolve は observed と accepted の差を、デバウンスの条件を満たしたものから確定させる。
//
// 最初の変化はすぐ確定させ、そこから Debounce の間は同じ役割の変化を見送る。
// 接点のバウンスで 1 回転が 2 回数えられるのを防ぐ一方、本物のパルスは
// 先頭で確実に数えられる。見送っている間に信号が落ち着いたら、ロックアウトが
// 明けた時点で実際の値に追いつく。
func (e *Engine) resolve(at time.Duration, wall time.Time) bool {
	// 役割の順序を固定する。旧実装が回転・大当り・電サポの順に処理していたのと
	// 同じ並びにしておく。同じ入力に対して常に同じ結果が出るようにするため。
	order := []signal.Role{
		signal.RoleStart,
		signal.RoleBonus,
		signal.RoleDensapo,
		signal.RolePayoutBonus,
		signal.RolePayout,
	}

	// エッジに渡す「今出ている信号の集合」は、この回で確定する分を反映した後の
	// 状態にする。旧実装が変化後のポート値をそのまま各エッジに渡していたのと
	// 揃えるため、まず確定する役割を決めてから状態を作る。
	type pending struct {
		role   signal.Role
		rising bool
	}

	var confirmed []pending
	for _, role := range order {
		if !e.wiring.Has(role) {
			continue
		}
		want := e.observed[role]
		if want == e.accepted[role] {
			continue
		}
		if e.ops.Debounce > 0 {
			if last, ok := e.acceptedAt[role]; ok && at-last < e.ops.Debounce {
				continue
			}
		}
		confirmed = append(confirmed, pending{role: role, rising: want})
	}

	if len(confirmed) == 0 {
		return false
	}

	// エッジを適用する前の状態を控えておく。初当たりでは大当り信号と電サポ信号が
	// 同時に立つため、プラグインはこれを見て初当たりと連荘を区別する。
	before := machine.RoleSet{}
	for role, on := range e.accepted {
		before[role] = on
	}

	for _, c := range confirmed {
		e.accepted[c.role] = c.rising
		e.acceptedAt[c.role] = at
	}

	active := machine.RoleSet{}
	for role, on := range e.accepted {
		active[role] = on
	}

	for _, c := range confirmed {
		edge := machine.Edge{
			Role:   c.role,
			Rising: c.rising,
			At:     at,
			Wall:   wall,
			Active: active,
			Before: before,
		}

		// 玉の増減は機種によらない計算なのでコア側で行う。プラグインは
		// カウンタと状態だけを受け持つ。
		e.updateBalls(edge)
		e.plugin.Edge(edge, &e.counters)
	}
	return true
}

// decode はポートの生値を役割ごとの状態に解く。アクティブローの反転はここで行う。
func (e *Engine) decode(ports signal.Ports) machine.RoleSet {
	out := machine.RoleSet{}
	for role, bit := range e.wiring.Bits {
		on := ports.Bit(bit)
		if e.wiring.ActiveLow {
			on = !on
		}
		out[role] = on
	}
	return out
}

// updateBalls は玉の増減を進める。
func (e *Engine) updateBalls(edge machine.Edge) {
	switch {
	case edge.Role == signal.RolePayout && edge.Rising:
		// 賞球信号が配線されている台では、獲得玉数が実測になる。
		e.counters.PayoutPulses++
		e.ballsGained += float64(signal.BallsPerPayoutPulse)

	case edge.Role == signal.RoleStart && edge.Rising:
		e.advanceFiring(edge)

	case edge.Role == signal.RoleBonus && !edge.Rising:
		// 賞球信号が無い台では、大当りの出玉を機種スペックの平均値で補う。
		// 平均値が無ければ何も足さない。分からない数字を作らない。
		if !e.PayoutWired() && e.spec.BonusPayoutAvg > 0 {
			e.ballsGained += float64(e.spec.BonusPayoutAvg)
		}
	}
}

// advanceFiring は 1 回転ぶんの打ち出しと戻りを進める。
func (e *Engine) advanceFiring(edge machine.Edge) {
	if !e.haveRotation {
		e.lastRotationAt = edge.At
		e.haveRotation = true
		return
	}

	dt := (edge.At - e.lastRotationAt).Seconds()
	e.lastRotationAt = edge.At
	e.secPerRotation = dt

	// 上限を超えた間隔は離席していたものとして切る。大当り中はスタート信号が
	// 出ないため、大当りを挟んだ間隔もここで切られる。
	if max := e.ops.MaxSecPerRotation; max > 0 && dt > max {
		dt = max
	}
	if dt <= 0 {
		return
	}

	spent := e.tuning.FireRate * dt
	e.ballsSpent += spent

	if e.PayoutWired() {
		// 獲得は実測なので、ここでは推定しない。
		return
	}

	e.ballsGained += (e.tuning.FireRate + e.netRate(edge.Active)) * dt
}

// netRate はその状態での玉の増減速度（玉/秒）を返す。負なら減っている。
//
// 旧実装の calcLpsOnNorm / calcLpsOnChance と同じ式である。通常時は
// 「250 個打って RotationRate 回転し、1 回転ごとにヘソ賞球が戻る」という
// 釣り合いから戻り率を出し、電サポ中は玉持ち率をそのまま使う。
func (e *Engine) netRate(active machine.RoleSet) float64 {
	if active.Has(signal.RoleDensapo) {
		return e.tuning.FireRate * (-1.0 + e.tuning.DensapoBase)
	}

	returned := float64(e.spec.StartPayout) * e.tuning.RotationRate
	if returned <= 0 {
		return -e.tuning.FireRate
	}
	return e.tuning.FireRate * (-1.0 + returned/(ballsPer250+returned))
}

// Correct はカウンタを手動補正する。共通スキーマのカウンタだけを許す。
//
// 機種プラグインの内部状態まで外から触れるようにすると、プラグインが自分の
// 不変条件を守れなくなる。詳細は
// docs/adr/0008-corrections-are-log-events.md を参照。
func (e *Engine) Correct(name string, delta int) error {
	target, ok := e.correctable()[name]
	if !ok {
		return fmt.Errorf("カウンタ %q は補正できません（補正できるカウンタ: %v）", name, CorrectableNames())
	}

	if *target+delta < 0 {
		return fmt.Errorf("カウンタ %q を %d だけ動かすと負になります（現在 %d）", name, delta, *target)
	}
	*target += delta
	return nil
}

func (e *Engine) correctable() map[string]*int {
	return map[string]*int{
		"current_rotations": &e.counters.CurrentRotations,
		"normal_rotations":  &e.counters.NormalRotations,
		"densapo_rotations": &e.counters.DensapoRotations,
		"bonuses":           &e.counters.Bonuses,
		"first_hits":        &e.counters.FirstHits,
		"chain":             &e.counters.Chain,
	}
}

// CorrectableNames は補正できるカウンタの名前を昇順で返す。
func CorrectableNames() []string {
	return []string{
		"bonuses",
		"chain",
		"current_rotations",
		"densapo_rotations",
		"first_hits",
		"normal_rotations",
	}
}

// Counters は現在のカウンタを返す。
func (e *Engine) Counters() machine.Counters { return e.counters }

// Snapshot はフロントへ渡す集計結果を組み立てる。
func (e *Engine) Snapshot() api.Snapshot {
	derived := api.Derived{
		TotalRotations:      e.counters.TotalRotations(),
		BallsGained:         e.ballsGained,
		BallsGainedMeasured: e.PayoutWired(),
		BallsSpent:          e.ballsSpent,
		BallsHeld:           e.ballsGained - e.ballsSpent,
		SecPerRotation:      e.secPerRotation,
	}
	if e.counters.FirstHits > 0 {
		derived.FirstHitRate = float64(e.counters.NormalRotations) / float64(e.counters.FirstHits)
	}

	metrics := e.plugin.Metrics(&e.counters)
	if metrics == nil {
		metrics = []machine.Metric{}
	}

	return api.Snapshot{
		Schema: api.SchemaVersion,
		Machine: api.MachineInfo{
			ID:          e.spec.ID,
			Variant:     e.spec.Variant,
			DisplayName: e.spec.DisplayName,
		},
		State:    e.plugin.State(&e.counters),
		Counters: e.counters,
		Derived:  derived,
		Metrics:  metrics,
	}
}
