// Package machine は機種プラグインの公開契約を定める。
//
// 機種プラグインはある機種の仕様を表現するものであり、信号の意味、カウンタの増減規則、
// 状態の遷移、そしてその機種固有の呼び名（「STEALTH RUSH」など）までを受け持つ。
// 色・フォント・レイアウト・画像は持たない。詳細は
// docs/adr/0002-plugin-owns-domain-front-owns-look.md を参照。
package machine

import (
	"time"

	"github.com/yukkeorg/pachicounter2/pkg/signal"
)

// StateKind は状態層の区分。機種を知らないフロントでも色や演出を決められるように、
// 全機種で共通の語彙に寄せてある。機種固有の呼び名は State.Label が持つ。
type StateKind string

const (
	// StateNormal は通常。通常確率で抽選されている。
	StateNormal StateKind = "normal"

	// StateBonus は大当り中。
	StateBonus StateKind = "bonus"

	// StateKakuhen は確変。大当り確率が通常より高い状態で、ST を含む。
	// ここでの回転は初当たり確率の分母に入らない。
	StateKakuhen StateKind = "kakuhen"

	// StateJitan は時短。電サポは付くが大当り確率は通常のままの状態。
	// ここでの回転は初当たり確率の分母に入る。
	StateJitan StateKind = "jitan"
)

// State は機種プラグインが解釈した現在の状態。
type State struct {
	// Kind は状態層の区分。フロントはこれで見た目を決める。
	Kind StateKind `json:"kind"`

	// Label は機種固有の呼び名。「STEALTH RUSH」「UFO RUSH」など。
	// フロントはこれをそのまま表示する。テーマを書いていない機種でも壊れない。
	Label string `json:"label"`
}

// Metric は機種固有の数値。共通スキーマに載らない数字はすべてこの形で渡すため、
// 機種を知らない汎用フロントでも並べるだけで表示できる。
type Metric struct {
	Key    string  `json:"key"`
	Label  string  `json:"label"`
	Value  float64 `json:"value"`
	Unit   string  `json:"unit,omitempty"`
	Digits int     `json:"digits,omitempty"`
}

// BonusRecord は連荘 1 回分の記録。連荘ごとの回転数は特定機種固有の概念ではなく
// 全機種で意味があるため、機種固有メトリクスではなく共通スキーマに置く。
type BonusRecord struct {
	// Chain はその大当りが連荘の何回目だったか。初当たりが 1 回目になる。
	Chain int `json:"chain"`

	// Rotations はその大当りを引くまでに要した回転数。
	Rotations int `json:"rotations"`

	// At はセッション開始からの経過時間。JSON にはナノ秒で載る。
	At time.Duration `json:"at_ns"`
}

// Counters は全機種に共通の集計値。コアが所有し、機種プラグインが増減させる。
//
// どの回転を NormalRotations に算入するか、FirstHits に何を数えるかは機種ごとに
// 異なって正しい。電サポ信号からは「玉が減らない状態にいる」ことしか分からず、
// その区間が通常確率かどうかは機種の仕様知識だからである。
// 詳細は docs/adr/0004-per-machine-denominator-rule.md を参照。
type Counters struct {
	// CurrentRotations は最後の大当り以降の回転数。
	CurrentRotations int `json:"current_rotations"`

	// NormalRotations は通常確率で抽選された回転の累計。初当たり確率の唯一の分母。
	NormalRotations int `json:"normal_rotations"`

	// DensapoRotations は電サポ中に消化した回転の累計。
	DensapoRotations int `json:"densapo_rotations"`

	// Bonuses は大当り回数。
	Bonuses int `json:"bonuses"`

	// FirstHits は初当たり回数。初当たり確率の分子。
	FirstHits int `json:"first_hits"`

	// Chain は現在の連荘数。
	Chain int `json:"chain"`

	// PayoutPulses は賞球信号のパルス数。賞球線を配線していない台では常に 0 で、
	// 獲得玉数はモデル推定に落ちる。
	PayoutPulses int `json:"payout_pulses"`

	// BonusHistory は連荘の履歴。新しいものが後ろに来る。
	BonusHistory []BonusRecord `json:"bonus_history,omitempty"`
}

// TotalRotations は総回転数を返す。通常時回転数と電サポ中回転数の和であり、
// 導出値なのでそれ自体を積算しない。
func (c Counters) TotalRotations() int {
	return c.NormalRotations + c.DensapoRotations
}

// RoleSet はある時点で出ている信号の集合。
type RoleSet map[signal.Role]bool

// Has は role の信号が出ているかを返す。
func (s RoleSet) Has(role signal.Role) bool {
	return s[role]
}

// Edge は信号の立上りまたは立下り。
//
// Active はそのエッジの時点で出ている信号の集合を含む。電サポ信号は大当り中も
// 同時に出ているため、機種プラグインはこれを見て区間の性質を判断する。
type Edge struct {
	// Role は変化した信号の役割。
	Role signal.Role

	// Rising は立上りなら true、立下りなら false。
	Rising bool

	// At はセッション開始からの経過時間。単調増加クロック基準。
	At time.Duration

	// Wall は記録用の実時刻。計算には使わない。
	Wall time.Time

	// Active はこのエッジの時点で出ている信号の集合。Role 自身も含む
	// （立上りなら true、立下りなら false）。
	Active RoleSet

	// Before は同時に確定したエッジを適用する前の信号の集合。
	//
	// 電サポ信号は大当り中も出ているため、初当たりでは大当り信号と電サポ信号が
	// 同時に立つ。Active だけでは初当たりと連荘中の大当りを区別できないので、
	// 「この大当りの前に電サポ中だったか」を Before で判断する。
	Before RoleSet
}

// Spec は機種プラグインが自分について宣言する情報。
type Spec struct {
	// ID は機種の識別子。起動引数とデータの保存先に使う。
	ID string

	// Variant はスペック違いの識別子。区別が無い機種では空文字列。
	Variant string

	// DisplayName は人間に見せる機種名。「CR STEALTH block.III」など。
	DisplayName string

	// RequiredRoles はこの機種の集計に必要な信号の役割。配線がこれを満たして
	// いなければ、コアは動き出す前にエラーで終了する。
	RequiredRoles []signal.Role

	// OptionalRoles は配線されていれば使う信号の役割。賞球信号のように、
	// 無ければ推定に落ちるものが入る。
	OptionalRoles []signal.Role

	// StartPayout はヘソ賞球数。玉の増減を推定するのに使う機種スペック。
	StartPayout int

	// BonusPayoutAvg は大当り 1 回あたりの平均出玉。賞球信号を配線していない台で
	// 大当りの出玉を推定するのに使う。0 なら推定せず、大当り中の玉の増減を
	// 勝手に作らない。
	//
	// 台が発射玉数の信号を出していないため、賞球信号が無い状態での持玉は
	// どうしても推定になる。分からない数字を作るより、分からないままにする。
	BonusPayoutAvg int
}

// Plugin は機種プラグイン。
//
// 動的ロードはしない。実装は init() でレジストリに自己登録し、コンパイル時に
// バイナリへ含まれる。詳細は docs/adr/0003-compile-time-plugin-registration.md を参照。
type Plugin interface {
	// Spec は自分について宣言する情報を返す。
	Spec() Spec

	// Reset は内部状態を初期状態に戻す。新しいセッションの開始時に呼ばれる。
	Reset()

	// Baseline は信号源に繋がった時点で出ている信号を伝える。
	//
	// カウンタを動かしてはならない。これはエッジではなく、単に「今こうなって
	// いる」という基準である。大当り中にコアを起動したとき、大当り回数を
	// 増やさずに状態表示だけを合わせるために使う。詳細は
	// docs/adr/0009-signal-baseline-and-device-loss.md を参照。
	Baseline(active RoleSet)

	// Edge は信号の立上り・立下りごとに呼ばれ、c を増減させる。
	// 初回読み取りの基準イベントでは呼ばれない。
	Edge(e Edge, c *Counters)

	// State は現在の状態を返す。
	State(c *Counters) State

	// Metrics は機種固有の数値を返す。共通スキーマに載る値は含めない。
	Metrics(c *Counters) []Metric
}
