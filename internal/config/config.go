// Package config はコアの設定を表す。
//
// 設定は 3 種類に分かれる。機種スペックは機種プラグインが持つのでここには無い。
// ここに入るのは台の調整値（その台・その日に固有の値）と運用パラメータ
// （ソフトの動かし方の値）、そして配線（ビット位置と役割の対応）である。
package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yukkeorg/pachicounter2/pkg/signal"
)

// Wiring はビット位置と信号の役割の対応。
//
// 配線はあなたの回路に固有で、機種を変えても変わらない。一方どの役割が必要かは
// 機種プラグインが宣言する。コアは起動時に、プラグインが要求する役割が全部
// 配線されているかを検証する。
type Wiring struct {
	// Bits は役割からビット位置への対応。
	Bits map[signal.Role]int

	// ActiveLow は信号が出ている間にビットが 0 になる配線かどうか。
	//
	// 台の外部出力端子はオープンコレクタで、プルアップ抵抗と組み合わせると
	// 信号が出ている間 LOW になる。旧実装がポート値を丸ごと反転していたのは
	// これが理由である。反転をここで扱うことで、生信号ログにはデバイスが返した
	// 値がそのまま残り、記録が観測した事実そのものになる。
	ActiveLow bool
}

// ParseWiring は "start=0,bonus=1,densapo=2" の形の指定を解釈する。
func ParseWiring(spec string) (Wiring, error) {
	w := Wiring{Bits: map[signal.Role]int{}}

	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		name, value, ok := strings.Cut(part, "=")
		if !ok {
			return Wiring{}, fmt.Errorf("配線の指定 %q は role=bit の形ではありません", part)
		}

		role := signal.Role(strings.TrimSpace(name))
		if !role.Valid() {
			return Wiring{}, fmt.Errorf("信号の役割 %q は定義されていません（使える役割: %v）", role, signal.AllRoles())
		}

		bit, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return Wiring{}, fmt.Errorf("配線 %q のビット位置を解釈できません: %w", part, err)
		}
		if bit < 0 || bit >= 16 {
			return Wiring{}, fmt.Errorf("配線 %q のビット位置が範囲外です（0〜15）", part)
		}

		if existing, dup := w.Bits[role]; dup {
			return Wiring{}, fmt.Errorf("役割 %q が二重に指定されています（ビット %d と %d）", role, existing, bit)
		}
		for other, otherBit := range w.Bits {
			if otherBit == bit {
				return Wiring{}, fmt.Errorf("ビット %d に役割 %q と %q の両方が割り当てられています", bit, other, role)
			}
		}
		w.Bits[role] = bit
	}

	if len(w.Bits) == 0 {
		return Wiring{}, fmt.Errorf("配線が 1 つも指定されていません")
	}
	return w, nil
}

// Spec は -wire にそのまま渡せる形で配線を書き出す。
func (w Wiring) Spec() string {
	roles := make([]string, 0, len(w.Bits))
	for role := range w.Bits {
		roles = append(roles, string(role))
	}
	sort.Strings(roles)

	parts := make([]string, 0, len(roles))
	for _, role := range roles {
		parts = append(parts, fmt.Sprintf("%s=%d", role, w.Bits[signal.Role(role)]))
	}

	return strings.Join(parts, ",")
}

// String は人が読む形で配線を書き出す。
func (w Wiring) String() string {
	s := w.Spec()
	if w.ActiveLow {
		s += "（アクティブロー）"
	}
	return s
}

// Equal は配線が同じかを返す。役割とビット位置の対応に加えて、アクティブローか
// どうかも比べる。どちらかが違えば、同じポート値から読み取る信号が変わる。
func (w Wiring) Equal(other Wiring) bool {
	if w.ActiveLow != other.ActiveLow || len(w.Bits) != len(other.Bits) {
		return false
	}
	for role, bit := range w.Bits {
		if otherBit, ok := other.Bits[role]; !ok || otherBit != bit {
			return false
		}
	}
	return true
}

// Has は役割が配線されているかを返す。
func (w Wiring) Has(role signal.Role) bool {
	_, ok := w.Bits[role]
	return ok
}

// Roles は配線されている役割を返す。
func (w Wiring) Roles() []signal.Role {
	out := make([]signal.Role, 0, len(w.Bits))
	for role := range w.Bits {
		out = append(out, role)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// DefaultWiring は現行の回路図どおりの配線を返す。
//
// usbio_counter.svg のラベルは Port0 の bit0 が回転数、bit1 が大当り、
// bit2 が「時短+大当り」である。4 本目（出玉あり大当り）は bit3 に繋ぐ。
func DefaultWiring() Wiring {
	return Wiring{
		Bits: map[signal.Role]int{
			signal.RoleStart:       0,
			signal.RoleBonus:       1,
			signal.RoleDensapo:     2,
			signal.RolePayoutBonus: 3,
		},
		ActiveLow: true,
	}
}

// Tuning は台の調整値。その台・その日に固有の値なので、コードには置かない。
type Tuning struct {
	// FireRate は打ち出し速度（玉/秒）。100 発/分なら 1.6667。
	FireRate float64

	// RotationRate は回転率。貸玉 250 個（4 円パチンコの千円分）あたりの回転数。
	// 台の釘調整で変わる。
	RotationRate float64

	// DensapoBase は電サポ中の玉持ち率。1 なら玉が減らない。
	DensapoBase float64
}

// DefaultTuning は旧実装が定数として持っていた値を返す。
//
// 旧実装の calcLpsOnNorm(3, 20) は打ち出し速度 1.6667、ヘソ賞球数 3、回転率 20 を
// 1 つの定数に潰していた。このうちヘソ賞球数は機種スペックなので機種プラグインへ、
// 残りは台の調整値としてここに置く。
func DefaultTuning() Tuning {
	return Tuning{
		FireRate:     1.6667,
		RotationRate: 20,
		DensapoBase:  0.8,
	}
}

// Ops は運用パラメータ。台にも機種にも属さない、ソフトの動かし方の値。
type Ops struct {
	// PollInterval はポーリング間隔。
	PollInterval time.Duration

	// Debounce は同一ビットの再エッジを無視する時間。接点のバウンスで
	// 1 回転が 2 回数えられるのを防ぐ。0 なら無効。
	Debounce time.Duration

	// MaxSecPerRotation は 1 回転あたりの秒数の上限。これを超えた間隔は
	// 離席していたものとして、玉の増減の計算から除外する。
	MaxSecPerRotation float64
}

// DefaultOps は既定の運用パラメータを返す。
func DefaultOps() Ops {
	return Ops{
		PollInterval:      5 * time.Millisecond,
		Debounce:          8 * time.Millisecond,
		MaxSecPerRotation: 40,
	}
}
