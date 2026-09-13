package opcmd

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yukkeorg/pachicounter2/api"
	"github.com/yukkeorg/pachicounter2/internal/counter"
	"github.com/yukkeorg/pachicounter2/internal/httpapi"
	"github.com/yukkeorg/pachicounter2/pkg/machine"
)

// fakeCore は本物の HTTP の窓口（httpapi）の裏に置く、偽のコア。
type fakeCore struct {
	mu       sync.Mutex
	snap     api.Snapshot
	err      error
	corrects []api.CorrectRequest
	notes    []string
}

func (f *fakeCore) Snapshot() api.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fakeCore) Subscribe() (<-chan api.Snapshot, func()) {
	return make(chan api.Snapshot), func() {}
}

func (f *fakeCore) NewSession(note string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.notes = append(f.notes, note)
	f.snap.Session.ID = "20260913-130000-stealth"
	f.snap.Counters = machine.Counters{}
	return nil
}

func (f *fakeCore) Correct(req api.CorrectRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.corrects = append(f.corrects, req)
	if req.Counter == "normal_rotations" {
		f.snap.Counters.NormalRotations += req.Delta
	}
	return nil
}

func sampleSnapshot() api.Snapshot {
	return api.Snapshot{
		Schema:  api.SchemaVersion,
		Machine: api.MachineInfo{ID: "stealth", DisplayName: "CR STEALTH block.III"},
		Session: api.SessionInfo{ID: "20260913-120000-stealth"},
		Device:  api.DeviceInfo{Source: "km2net USB-IO 2.0", Connected: true},
		State:   machine.State{Kind: machine.StateKakuhen, Label: "STEALTH RUSH"},
		Counters: machine.Counters{
			CurrentRotations: 45,
			NormalRotations:  309,
			DensapoRotations: 102,
			Bonuses:          4,
			FirstHits:        2,
		},
	}
}

type harness struct {
	core *fakeCore
	addr string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	core := &fakeCore{snap: sampleSnapshot()}
	server, err := httpapi.New(core, httpapi.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("HTTP の窓口を組み立てられません: %v", err)
	}
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)

	return &harness{core: core, addr: strings.TrimPrefix(ts.URL, "http://")}
}

type outcome struct {
	code     int
	stdout   string
	stderr   string
	notified []string
}

// run はサブコマンドを動かす。terminal は、端末から打ったか（ショートカットなどではないか）。
func run(terminal bool, stdin string, name string, args ...string) outcome {
	var stdout, stderr bytes.Buffer
	var notified []string
	env := Env{
		Stdin:            strings.NewReader(stdin),
		Stdout:           &stdout,
		Stderr:           &stderr,
		StdinIsTerminal:  terminal,
		StderrIsTerminal: terminal,
		Notify:           func(summary, body string) { notified = append(notified, summary+" "+body) },
		HTTPClient:       &http.Client{Timeout: 5 * time.Second},
	}
	code := Run(name, args, env)
	return outcome{code: code, stdout: stdout.String(), stderr: stderr.String(), notified: notified}
}

func TestCountersMatchCorrectableCounters(t *testing.T) {
	// 表示の語を持つカウンタと、コアが補正を受け付けるカウンタが食い違わないこと。
	got := counterNames()
	sort.Strings(got)
	if want := counter.CorrectableNames(); !reflect.DeepEqual(got, want) {
		t.Fatalf("操作コマンドのカウンタ = %v, コアが補正できるカウンタ = %v", got, want)
	}

	c := machine.Counters{CurrentRotations: 1, NormalRotations: 2, DensapoRotations: 3, Bonuses: 4, FirstHits: 5, Chain: 6}
	want := map[string]int{"current_rotations": 1, "normal_rotations": 2, "densapo_rotations": 3, "bonuses": 4, "first_hits": 5, "chain": 6}
	for name, v := range want {
		if got, ok := counterValue(c, name); !ok || got != v {
			t.Errorf("counterValue(%s) = %d, %v, 期待は %d", name, got, ok, v)
		}
	}
}

func TestParseDelta(t *testing.T) {
	ok := map[string]int{"+1": 1, "-1": -1, "+12": 12}
	for s, want := range ok {
		if got, err := parseDelta(s); err != nil || got != want {
			t.Errorf("parseDelta(%q) = %d, %v, 期待は %d", s, got, err, want)
		}
	}
	// 符号の無い数は、値の設定と取り違えやすいので断る。
	for _, s := range []string{"1", "310", "+0", "-0", "+x", ""} {
		if _, err := parseDelta(s); err == nil {
			t.Errorf("parseDelta(%q) がエラーにならない", s)
		}
	}
}

func TestCorrect(t *testing.T) {
	h := newHarness(t)

	out := run(true, "", "correct", "-core", h.addr, "normal_rotations", "+1", "取りこぼし")
	if out.code != exitOK {
		t.Fatalf("終了コード = %d, 出力: %s", out.code, out.stderr)
	}
	if out.stdout != "通常時回転数  309 → 310\n" {
		t.Errorf("出力 = %q", out.stdout)
	}
	want := []api.CorrectRequest{{Counter: "normal_rotations", Delta: 1, Note: "取りこぼし"}}
	if !reflect.DeepEqual(h.core.corrects, want) {
		t.Errorf("コアが受け取った補正 = %+v, 期待は %+v", h.core.corrects, want)
	}
}

func TestCorrectArgumentErrorsSendNothing(t *testing.T) {
	cases := map[string][]string{
		"符号が無い":      {"normal_rotations", "310"},
		"量が 0":       {"normal_rotations", "+0"},
		"用語集の語で指定した": {"通常時回転数", "+1"},
		"量が無い":       {"normal_rotations"},
		"引数が多すぎる":    {"normal_rotations", "+1", "取りこぼし", "配信中"},
		"後ろのオプション":   {"normal_rotations", "+1", "-core", "127.0.0.1:1"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			out := run(true, "", "correct", append([]string{"-core", h.addr}, args...)...)
			if out.code != exitUsage {
				t.Errorf("終了コード = %d, 期待は %d（出力: %s）", out.code, exitUsage, out.stderr)
			}
			if len(h.core.corrects) != 0 {
				t.Errorf("引数の誤りなのにコアに補正が送られた: %+v", h.core.corrects)
			}
			if !strings.HasPrefix(out.stderr, "エラー: ") {
				t.Errorf("エラーの出力 = %q", out.stderr)
			}
		})
	}

	out := run(true, "", "correct", "-core", "http://127.0.0.1:18888", "normal_rotations", "+1")
	if out.code != exitUsage {
		t.Errorf("-core に URL を渡したときの終了コード = %d, 期待は %d", out.code, exitUsage)
	}
}

func TestCoreRefusalIsReported(t *testing.T) {
	h := newHarness(t)
	h.core.err = errors.New("補正できません: 再生中は記録を変える操作を受け付けません")

	out := run(true, "", "correct", "-core", h.addr, "normal_rotations", "+1")
	if out.code != exitFail {
		t.Errorf("終了コード = %d, 期待は %d", out.code, exitFail)
	}
	if !strings.Contains(out.stderr, "再生中は記録を変える操作を受け付けません") {
		t.Errorf("コアが断った理由が出ていない: %q", out.stderr)
	}
}

func TestUnreachableCore(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("待ち受けられません: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	out := run(true, "", "status", "-core", addr)
	if out.code != exitFail {
		t.Errorf("終了コード = %d, 期待は %d", out.code, exitFail)
	}
	if !strings.Contains(out.stderr, "コアに接続できません") {
		t.Errorf("出力 = %q", out.stderr)
	}
}

func TestNotifyOnlyOnFailureOutsideTerminal(t *testing.T) {
	h := newHarness(t)

	if out := run(false, "", "correct", "-core", h.addr, "normal_rotations", "+1"); len(out.notified) != 0 {
		t.Errorf("成功したのに通知した: %v", out.notified)
	}
	if out := run(false, "", "correct", "-core", h.addr, "normal_rotations", "1"); len(out.notified) != 1 {
		t.Errorf("端末の外で失敗したのに通知の数 = %d", len(out.notified))
	}
	if out := run(true, "", "correct", "-core", h.addr, "normal_rotations", "1"); len(out.notified) != 0 {
		t.Errorf("端末で失敗したのに通知した: %v", out.notified)
	}
}

func TestSessionNewOutsideTerminalNeedsYes(t *testing.T) {
	h := newHarness(t)

	out := run(false, "", "session", "new", "-core", h.addr)
	if out.code != exitUsage {
		t.Errorf("-yes 無しの終了コード = %d, 期待は %d", out.code, exitUsage)
	}
	if len(h.core.notes) != 0 {
		t.Fatalf("-yes 無しで新しいセッションが始まった")
	}

	out = run(false, "", "session", "new", "-core", h.addr, "-yes", "台を入れ替え")
	if out.code != exitOK {
		t.Fatalf("-yes 付きの終了コード = %d, 出力: %s", out.code, out.stderr)
	}
	if !reflect.DeepEqual(h.core.notes, []string{"台を入れ替え"}) {
		t.Errorf("コアが受け取った覚書 = %q", h.core.notes)
	}
	if !strings.Contains(out.stdout, "20260913-130000-stealth") {
		t.Errorf("始めたセッションが出ていない: %q", out.stdout)
	}
}

func TestSessionNewConfirmsOnTerminal(t *testing.T) {
	h := newHarness(t)

	out := run(true, "\n", "session", "new", "-core", h.addr)
	if out.code != exitFail {
		t.Errorf("取りやめたときの終了コード = %d, 期待は %d", out.code, exitFail)
	}
	if len(h.core.notes) != 0 {
		t.Fatal("取りやめたのに新しいセッションが始まった")
	}
	if !strings.Contains(out.stderr, "20260913-120000-stealth") || !strings.Contains(out.stderr, "元には戻せません") {
		t.Errorf("確認に閉じるセッションが出ていない: %q", out.stderr)
	}

	out = run(true, "y\n", "session", "new", "-core", h.addr)
	if out.code != exitOK {
		t.Fatalf("y と答えたときの終了コード = %d, 出力: %s", out.code, out.stderr)
	}
	if len(h.core.notes) != 1 {
		t.Errorf("y と答えたのに新しいセッションが始まっていない")
	}
}

func TestStatus(t *testing.T) {
	h := newHarness(t)

	out := run(true, "", "status", "-core", h.addr)
	if out.code != exitOK {
		t.Fatalf("終了コード = %d, 出力: %s", out.code, out.stderr)
	}

	for _, want := range []string{"CR STEALTH block.III（STEALTH RUSH）", "20260913-120000-stealth", "km2net USB-IO 2.0（接続中）"} {
		if !strings.Contains(out.stdout, want) {
			t.Errorf("出力に %q が無い:\n%s", want, out.stdout)
		}
	}

	// カウンタの行は「用語集の語、API の名前、値」の順で、API の名前の桁が揃っていること。
	wantRows := map[string][2]string{
		"大当り間回転数": {"current_rotations", "45"},
		"通常時回転数":  {"normal_rotations", "309"},
		"電サポ中回転数": {"densapo_rotations", "102"},
		"大当り回数":   {"bonuses", "4"},
		"初当たり回数":  {"first_hits", "2"},
		"連荘数":     {"chain", "0"},
	}
	nameColumn := -1
	for _, line := range strings.Split(out.stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		want, ok := wantRows[fields[0]]
		if !ok {
			continue
		}
		delete(wantRows, fields[0])
		if fields[1] != want[0] || fields[2] != want[1] {
			t.Errorf("行 %q, 期待は %s %s", line, want[0], want[1])
		}
		col := displayWidth(line[:strings.Index(line, fields[1])])
		if nameColumn == -1 {
			nameColumn = col
		} else if col != nameColumn {
			t.Errorf("API の名前の桁が揃っていない: %q（%d 桁目、期待は %d 桁目）", line, col, nameColumn)
		}
	}
	if len(wantRows) != 0 {
		t.Errorf("出力に無いカウンタ: %v\n%s", wantRows, out.stdout)
	}
}

func TestStatusDuringReplay(t *testing.T) {
	h := newHarness(t)
	h.core.snap.Session.ID = ""

	out := run(true, "", "status", "-core", h.addr)
	if !strings.Contains(out.stdout, "再生中のため記録していません") {
		t.Errorf("再生中であることが出ていない:\n%s", out.stdout)
	}
}

func TestUnknownSubcommandAndHelp(t *testing.T) {
	if out := run(true, "", "corect", "normal_rotations", "+1"); out.code != exitUsage || !strings.Contains(out.stderr, "不明なサブコマンドです: corect") {
		t.Errorf("知らないサブコマンド: 終了コード %d, 出力 %q", out.code, out.stderr)
	}
	if out := run(true, "", "session"); out.code != exitUsage {
		t.Errorf("session だけのときの終了コード = %d", out.code)
	}
	if out := run(true, "", "session", "old"); out.code != exitUsage {
		t.Errorf("session old のときの終了コード = %d", out.code)
	}
	for _, args := range [][]string{{"status", "-h"}, {"correct", "-h"}, {"session", "-h"}, {"session", "new", "-h"}} {
		if out := run(true, "", args[0], args[1:]...); out.code != exitOK || !strings.Contains(out.stderr, "使い方:") {
			t.Errorf("%q: 終了コード %d, 出力 %q", args, out.code, out.stderr)
		}
	}
}

func TestDisplayWidth(t *testing.T) {
	cases := map[string]int{"abc": 3, "大当り間回転数": 14, "（接続中）": 10, "セッション": 10}
	for s, want := range cases {
		if got := displayWidth(s); got != want {
			t.Errorf("displayWidth(%q) = %d, 期待は %d", s, got, want)
		}
	}
}
