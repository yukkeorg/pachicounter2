// Package signal は信号源の公開契約を定める。
//
// 信号源は台の信号をコアに届けるものであり、USB-HID の GPIO デバイス、Raspberry Pi の
// GPIO、記録した生信号の再生、テスト用のダミーはすべて信号源である。コアから見て
// これらの区別はない。詳細は docs/adr/0005-signal-source-abstraction.md を参照。
package signal

import (
	"context"
	"time"
)

// Role は信号の役割を表す。台が出力している電気的事実だけを指し、台の内部で何が
// 起きているかの解釈は含まない。解釈は機種プラグインの責務である。
type Role string

const (
	// RoleStart はスタート信号。特別図柄の停止ごとに 1 パルス出る。1 パルス＝1 回転。
	RoleStart Role = "start"

	// RoleBonus は大当り信号。大当り中に出ている。
	RoleBonus Role = "bonus"

	// RoleDensapo は電サポ信号。電サポ（高ベース）中に出ているが、大当り中も同時に
	// 出ているため、この信号だけでは「玉が減らない状態にいる」ことしか分からない。
	// その区間が高確率（確変・ST）なのか通常確率（時短）なのかは判別できない。
	RoleDensapo Role = "densapo"

	// RolePayoutBonus は出玉あり大当り信号。出玉を伴う大当り（＝終了後に電サポへ
	// 繋がる当り）の間だけ出ている。大当り信号が出玉なしの大当りも含めて出るのに
	// 対し、こちらは実質の当りだけを拾える。
	RolePayoutBonus Role = "payout_bonus"

	// RolePayout は賞球信号。賞球 10 個の払い出しごとに 1 パルス出る。配線は任意で、
	// 繋いでいない台では獲得玉数がモデル推定に落ちる。
	RolePayout Role = "payout"
)

// BallsPerPayoutPulse は賞球信号 1 パルスが表す玉数。
const BallsPerPayoutPulse = 10

// AllRoles は定義済みの役割をすべて返す。設定の検証と一覧表示に使う。
func AllRoles() []Role {
	return []Role{RoleStart, RoleBonus, RoleDensapo, RolePayoutBonus, RolePayout}
}

// Valid は定義済みの役割かどうかを返す。
func (r Role) Valid() bool {
	for _, known := range AllRoles() {
		if r == known {
			return true
		}
	}
	return false
}

// Ports は信号源が読んだポートの生値。ビット位置と役割の対応は台ごとの配線で
// 決まるため、この型は解釈を持たない。
type Ports uint16

// Bit はビット位置 n の状態を返す。
func (p Ports) Bit(n int) bool {
	if n < 0 || n >= 16 {
		return false
	}
	return p&(1<<uint(n)) != 0
}

// Event は信号源が観測した 1 回の変化。
type Event struct {
	// At は信号源が動き出してからの経過時間。単調増加クロックを基準とするため、
	// 実時刻の補正（NTP など）に影響されない。経過時間の計算には必ずこちらを使う。
	At time.Duration `json:"at"`

	// Wall は記録と振り返りのための実時刻。計算には使わない。
	Wall time.Time `json:"wall"`

	// Ports は変化後のポートの生値。
	Ports Ports `json:"ports"`

	// Baseline はこのイベントが基準イベントであることを表す。信号源に繋がった
	// 直後や再接続の直後に一度だけ true になる。受け手はこれをエッジとして
	// 扱ってはならない。起動時に立っているビットを立上りと数えると、大当り中に
	// コアを起動しただけで大当り回数が増える。詳細は
	// docs/adr/0009-signal-baseline-and-device-loss.md を参照。
	Baseline bool `json:"baseline,omitempty"`
}

// Source は信号源。Events が返すチャネルにイベントが流れる。
//
// ポーリングするかエッジ割り込みを待つかは実装の内部事情であり、この契約には
// 現れない。ポーリング型に固定すると、カーネルのエッジ割り込みで通知できる
// Raspberry Pi GPIO の利点や、マイコン側から push する形が押し潰される。
type Source interface {
	// Name は信号源の名前を返す。表示とログに使う。
	Name() string

	// Events はイベントの流れを開始する。ctx の終了でチャネルは閉じる。
	// 同一の Source に対して複数回呼んではならない。
	//
	// 最初に流すイベントは、その時点のポート値を伝える基準イベントであり、
	// Event.Baseline が true になる。
	Events(ctx context.Context) (<-chan Event, error)

	// Connected は信号源が今この瞬間に台の信号を読めているかを返す。
	// デバイスが抜けていても信号源は生き続け、接続を待つ（ADR-0009）。
	Connected() bool
}
