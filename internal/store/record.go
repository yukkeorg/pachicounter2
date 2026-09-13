package store

import (
	"time"

	"github.com/yukkeorg/pachicounter2/pkg/signal"
)

// Kind は生信号ログに並ぶレコードの種別。
type Kind string

const (
	// KindSessionStart はセッションの開始。ログの 1 行目に必ず来る。
	KindSessionStart Kind = "session_start"

	// KindBaseline は基準イベント。信号源に繋がった時点のポート値であり、
	// エッジとして扱ってはならない。起動時や再接続時に立っているビットを
	// 立上りと数えると、大当り中にコアを起動しただけで大当り回数が増える。
	KindBaseline Kind = "baseline"

	// KindSignal はポート値の変化。
	KindSignal Kind = "signal"

	// KindCorrect は手動補正。スナップショットを直接書き換えるのではなく、
	// ここに記録して再集計でも再現させる。
	KindCorrect Kind = "correct"

	// KindDisconnect は信号源との接続が切れたこと。切れている間の変化は
	// 原理的に取り逃しているため、再接続後は新しい基準イベントから始まる。
	KindDisconnect Kind = "disconnect"

	// KindResume はコアを再起動してセッションを続けたこと。止まっていた間の変化は
	// 記録されていないので、続きは次の基準イベントから読み直す。
	KindResume Kind = "resume"
)

// Record は生信号ログの 1 行。
//
// このログが唯一の真実であり、集計結果はそこからの導出物である。集計規則を
// 直したあとに過去のセッションを再集計できるのはこの性質のおかげなので、
// 集計結果だけを書き換えるような経路は作らない。詳細は
// docs/adr/0006-raw-signal-log-plus-snapshot.md を参照。
type Record struct {
	// Kind はレコードの種別。
	Kind Kind `json:"type"`

	// At はセッション開始からの経過時間。単調増加クロック基準。
	At time.Duration `json:"at_ns"`

	// Wall は記録用の実時刻。計算には使わない。
	Wall time.Time `json:"wall"`

	// Ports は KindBaseline と KindSignal でのポートの生値。
	Ports *signal.Ports `json:"ports,omitempty"`

	// Session は KindSessionStart でのセッション識別子。
	Session string `json:"session,omitempty"`

	// Machine と Variant は KindSessionStart での機種。
	Machine string `json:"machine,omitempty"`
	Variant string `json:"variant,omitempty"`

	// Wiring は KindSessionStart での配線（役割とビット位置の対応）。
	// 同じログを後から再集計するとき、当時の配線が分からないと解釈できない。
	Wiring map[signal.Role]int `json:"wiring,omitempty"`

	// ActiveLow は KindSessionStart で、信号が出ている間にビットが 0 になる
	// 配線かどうか。
	ActiveLow bool `json:"active_low,omitempty"`

	// Counter と Delta と Note は KindCorrect での補正内容。
	Counter string `json:"counter,omitempty"`
	Delta   int    `json:"delta,omitempty"`
	Note    string `json:"note,omitempty"`
}
