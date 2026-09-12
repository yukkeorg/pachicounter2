// Package api はコアとフロントの間の HTTP 契約を定める。
//
// フロントは見た目だけを受け持ち、機種の仕様を知らない。機種固有の数値は Metrics に
// ラベル付きで載るので、機種を知らない汎用フロントでも並べるだけで表示できる。
// 詳細は docs/adr/0001-core-as-resident-http-server.md と
// docs/adr/0002-plugin-owns-domain-front-owns-look.md を参照。
package api

import (
	"time"

	"github.com/yukkeorg/pachicounter/pkg/machine"
)

// SchemaVersion はスナップショットの構造の版。互換性を壊す変更で上げる。
const SchemaVersion = 1

// エンドポイント。
const (
	// PathEvents は SSE でスナップショットを push する。
	PathEvents = "/events"

	// PathSnapshot は現在のスナップショットを 1 回返す。
	PathSnapshot = "/api/snapshot"

	// PathSessionNew は新しいセッションを開始する。
	PathSessionNew = "/api/session/new"

	// PathCorrect はカウンタを手動補正する。
	PathCorrect = "/api/correct"

	// PathMachines は登録済みの機種を一覧する。
	PathMachines = "/api/machines"
)

// EventName は SSE のイベント名。
const EventName = "snapshot"

// Snapshot はコアがフロントへ渡す集計結果の全体。差分ではなく毎回これを丸ごと送る。
type Snapshot struct {
	Schema   int              `json:"schema"`
	Machine  MachineInfo      `json:"machine"`
	Session  SessionInfo      `json:"session"`
	Device   DeviceInfo       `json:"device"`
	State    machine.State    `json:"state"`
	Counters machine.Counters `json:"counters"`
	Derived  Derived          `json:"derived"`
	Metrics  []machine.Metric `json:"metrics"`
}

// MachineInfo は今集計している機種。
type MachineInfo struct {
	ID          string `json:"id"`
	Variant     string `json:"variant,omitempty"`
	DisplayName string `json:"name"`
}

// SessionInfo は今のセッション。台に向かってから離れるまでの一区切りであり、
// 画面に出る数字はすべてこのセッション内の数字である。
type SessionInfo struct {
	ID         string    `json:"id"`
	StartedAt  time.Time `json:"started_at"`
	ElapsedSec float64   `json:"elapsed_sec"`
}

// DeviceInfo は信号源の状態。デバイスが抜けてもコアは生き続けるので、
// フロントは Connected を見て「今読めていない」ことを表示できる。
type DeviceInfo struct {
	Source    string `json:"source"`
	Connected bool   `json:"connected"`
}

// Derived は導出値。分母の規則が機種プラグインの責務である以上、フロントに
// 再計算させると規則がフロント側へ漏れるため、コアが計算して送る。
type Derived struct {
	// TotalRotations は総回転数。通常時回転数と電サポ中回転数の和。
	TotalRotations int `json:"total_rotations"`

	// FirstHitRate は初当たり確率 1/N の N。まだ初当たりが無ければ 0。
	FirstHitRate float64 `json:"first_hit_rate"`

	// BallsGained は獲得玉数。賞球線を配線している台では実測値。
	BallsGained float64 `json:"balls_gained"`

	// BallsGainedMeasured は獲得玉数が実測かどうか。
	BallsGainedMeasured bool `json:"balls_gained_measured"`

	// BallsSpent は消費玉数。台が発射数の信号を出していないため常に推定値。
	BallsSpent float64 `json:"balls_spent"`

	// BallsHeld は持玉。消費側が原理的に推定でしかないため、これも推定値。
	BallsHeld float64 `json:"balls_held"`

	// SecPerRotation は直近の 1 回転あたりの秒数。
	SecPerRotation float64 `json:"sec_per_rotation"`
}

// CorrectRequest はカウンタの手動補正。取りこぼしを 1 つ足すなど。
//
// 補正はスナップショットを直接書き換えるのではなく、生信号ログ上のイベントとして
// 記録され、再集計でも再現される。詳細は
// docs/adr/0008-corrections-are-log-events.md を参照。
type CorrectRequest struct {
	// Counter は補正するカウンタの名前。共通スキーマのカウンタだけを許す。
	Counter string `json:"counter"`

	// Delta は増減量。
	Delta int `json:"delta"`

	// Note は補正の理由。記録に残る。
	Note string `json:"note,omitempty"`
}

// SessionNewRequest は新しいセッションの開始。
type SessionNewRequest struct {
	// Note はセッションの覚書。記録に残る。
	Note string `json:"note,omitempty"`
}

// MachineListItem は登録済みの機種 1 件。
type MachineListItem struct {
	ID          string   `json:"id"`
	DisplayName string   `json:"name"`
	Variants    []string `json:"variants,omitempty"`
}

// ErrorResponse はエラー応答。
type ErrorResponse struct {
	Error string `json:"error"`
}
