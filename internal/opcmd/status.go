package opcmd

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"github.com/yukkeorg/pachicounter2/api"
	"github.com/yukkeorg/pachicounter2/pkg/machine"
)

// counters は補正できるカウンタを表示する順に並べる。Name は API と生信号ログで使う
// 名前で、引数にはこれを使う。Label は用語集の語で、表示にはこれを使う。
var counters = []struct {
	Name  string
	Label string
}{
	{Name: "current_rotations", Label: "大当り間回転数"},
	{Name: "normal_rotations", Label: "通常時回転数"},
	{Name: "densapo_rotations", Label: "電サポ中回転数"},
	{Name: "bonuses", Label: "大当り回数"},
	{Name: "first_hits", Label: "初当たり回数"},
	{Name: "chain", Label: "連荘数"},
}

func counterLabel(name string) (string, bool) {
	for _, c := range counters {
		if c.Name == name {
			return c.Label, true
		}
	}
	return "", false
}

func counterNames() []string {
	out := make([]string, 0, len(counters))
	for _, c := range counters {
		out = append(out, c.Name)
	}
	return out
}

func counterValue(c machine.Counters, name string) (int, bool) {
	switch name {
	case "current_rotations":
		return c.CurrentRotations, true
	case "normal_rotations":
		return c.NormalRotations, true
	case "densapo_rotations":
		return c.DensapoRotations, true
	case "bonuses":
		return c.Bonuses, true
	case "first_hits":
		return c.FirstHits, true
	case "chain":
		return c.Chain, true
	default:
		return 0, false
	}
}

// printStatus は、正しい台・正しいセッションに対して操作しているかを確かめるための
// 文脈と、補正できるカウンタを書く。導出値や推定値、機種固有の数字はフロントに任せる。
func printStatus(w io.Writer, snap api.Snapshot) {
	machineLine := snap.Machine.DisplayName
	if snap.State.Label != "" {
		machineLine += "（" + snap.State.Label + "）"
	}
	session := snap.Session.ID
	if session == "" {
		session = "（再生中のため記録していません）"
	}
	device := snap.Device.Source
	if snap.Device.Connected {
		device += "（接続中）"
	} else {
		device += "（未接続）"
	}

	head := [][2]string{{"機種", machineLine}, {"セッション", session}, {"信号源", device}}
	headWidth := 0
	for _, h := range head {
		headWidth = max(headWidth, displayWidth(h[0]))
	}
	for _, h := range head {
		fmt.Fprintf(w, "%s  %s\n", padRight(h[0], headWidth), h[1])
	}
	fmt.Fprintln(w)

	values := make([]string, len(counters))
	labelWidth, nameWidth, valueWidth := 0, 0, 0
	for i, c := range counters {
		v, _ := counterValue(snap.Counters, c.Name)
		values[i] = strconv.Itoa(v)
		labelWidth = max(labelWidth, displayWidth(c.Label))
		nameWidth = max(nameWidth, len(c.Name))
		valueWidth = max(valueWidth, len(values[i]))
	}
	for i, c := range counters {
		fmt.Fprintf(w, "%s  %-*s  %*s\n", padRight(c.Label, labelWidth), nameWidth, c.Name, valueWidth, values[i])
	}
}

// displayWidth は端末での表示幅を返す。漢字・かな・全角の記号は 2 桁として数える。
// 表示するのは用語集の語と機種名なので、この範囲で足りる。
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		if isWide(r) {
			w += 2
		} else {
			w++
		}
	}
	return w
}

func isWide(r rune) bool {
	switch {
	case unicode.Is(unicode.Han, r):
		return true
	case r >= 0x3000 && r <= 0x30FF: // 全角の句読点・記号、ひらがな、カタカナ
		return true
	case r >= 0xFF01 && r <= 0xFF60: // 全角の英数字と括弧
		return true
	case r >= 0xFFE0 && r <= 0xFFE6:
		return true
	default:
		return false
	}
}

// padRight は s の後ろに空白を足して、表示幅を width に揃える。
func padRight(s string, width int) string {
	if pad := width - displayWidth(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}
