package hidpin

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yukkeorg/hidpin/host-go/hidpin"
	"github.com/yukkeorg/hidpin/host-go/hidpin/hidpintest"
	"github.com/yukkeorg/pachicounter2/internal/config"
	"github.com/yukkeorg/pachicounter2/pkg/signal"
)

// allHigh は監視ピンがすべて HIGH のポート値。偽のボードは Raspberry Pi Pico で、既定の
// ピン設定は使えるピンをすべてプルアップ入力にするので、何も繋いでいなければこうなる。
const allHigh = signal.Ports(hidpintest.PicoAvailable)

// low は allHigh から、指定したビットを落としたポート値。
func low(bits ...int) signal.Ports {
	p := allHigh
	for _, b := range bits {
		p &^= 1 << b
	}
	return p
}

// harness は信号源を偽のバスにつないで動かす。
type harness struct {
	t      *testing.T
	src    *Source
	events <-chan signal.Event
	cancel context.CancelFunc
	lastAt time.Duration
	logs   *logBuffer
}

// logBuffer は信号源のログを溜める。ログは信号源のゴルーチンが書くので、鍵をかける。
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func start(t *testing.T, bus hidpin.Bus, opts Options) *harness {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	logs := &logBuffer{}
	opts.Logger = slog.New(slog.NewTextHandler(logs, nil))
	opts.bus = bus
	if opts.ScanInterval == 0 {
		opts.ScanInterval = 10 * time.Millisecond
	}

	src := New(opts)
	events, err := src.Events(ctx)
	if err != nil {
		t.Fatalf("Events がエラー: %v", err)
	}
	return &harness{t: t, src: src, events: events, cancel: cancel, logs: logs}
}

// next は次のイベントを受け取る。時刻が後戻りしないことも確かめる。
func (h *harness) next() signal.Event {
	h.t.Helper()
	select {
	case ev, ok := <-h.events:
		if !ok {
			h.t.Fatal("イベントの流れが終わっています")
		}
		if ev.At < h.lastAt {
			h.t.Errorf("時刻が後戻りしました: %v → %v", h.lastAt, ev.At)
		}
		h.lastAt = ev.At
		return ev
	case <-time.After(3 * time.Second):
		h.t.Fatal("イベントが届きません")
		return signal.Event{}
	}
}

// expect は次のイベントが期待どおりかを確かめる。
func (h *harness) expect(what string, ports signal.Ports, baseline bool) signal.Event {
	h.t.Helper()
	ev := h.next()
	if ev.Ports != ports || ev.Baseline != baseline {
		h.t.Fatalf("%s: {ports:%#x baseline:%v}, 期待は {ports:%#x baseline:%v}",
			what, uint32(ev.Ports), ev.Baseline, uint32(ports), baseline)
	}
	return ev
}

// quiet はしばらくイベントが来ないことを確かめる。
func (h *harness) quiet() {
	h.t.Helper()
	select {
	case ev := <-h.events:
		h.t.Fatalf("余計なイベントが流れました: {ports:%#x baseline:%v}", uint32(ev.Ports), ev.Baseline)
	case <-time.After(200 * time.Millisecond):
	}
}

// waitFor は cond が成り立つまで待つ。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s になりません", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestEdgesBecomeEvents(t *testing.T) {
	board := hidpintest.NewBoard("SERIAL1")
	h := start(t, hidpintest.NewBus(board), Options{})

	h.expect("最初のイベント", allHigh, true)

	// スタート信号のパルス。オープンコレクタなので、信号が出ている間は LOW になる。
	board.SetInput(0, false)
	h.expect("立下り", low(0), false)
	board.SetInput(0, true)
	h.expect("立上り", allHigh, false)

	// 上位のビットに当たる GPIO28。
	board.SetInput(28, false)
	h.expect("GPIO28 の変化", low(28), false)

	// 電気的な値が変わらない指定では、変化が起きない。
	board.SetInput(28, false)
	h.quiet()

	if got, want := h.src.Name(), "hidpin (Raspberry Pi Pico)"; got != want {
		t.Errorf("Name() = %q, 期待は %q", got, want)
	}
	if !h.src.Connected() {
		t.Error("Connected() が false")
	}
}

func TestSerialPicksTheDevice(t *testing.T) {
	other := hidpintest.NewBoard("SERIAL1")
	want := hidpintest.NewBoard("SERIAL2")
	h := start(t, hidpintest.NewBus(other, want), Options{Serial: "SERIAL2"})

	h.expect("最初のイベント", allHigh, true)

	// 指定した方のデバイスだけを読んでいること。
	other.SetInput(0, false)
	h.quiet()
	want.SetInput(0, false)
	h.expect("立下り", low(0), false)
}

func TestReplugRebaselines(t *testing.T) {
	board := hidpintest.NewBoard("SERIAL1")
	bus := hidpintest.NewBus(board)
	h := start(t, bus, Options{})

	h.expect("最初のイベント", allHigh, true)

	bus.Unplug(board)
	waitFor(t, "切断", func() bool { return !h.src.Connected() })

	// 抜けている間の変化は取り逃している。挿し直したら、今の値を基準にし直すこと。
	board.SetInput(1, false)
	bus.Plug(board)

	h.expect("挿し直した後", low(1), true)
	waitFor(t, "再接続", func() bool { return h.src.Connected() })

	board.SetInput(1, true)
	h.expect("立上り", allHigh, false)
}

func TestReplugWithoutChangeStillRebaselines(t *testing.T) {
	board := hidpintest.NewBoard("SERIAL1")
	bus := hidpintest.NewBus(board)
	h := start(t, bus, Options{})

	h.expect("最初のイベント", allHigh, true)

	// 抜き挿しの間に値が変わっていなくても、切れている間の変化は取り逃しているかも
	// しれない。基準を流し直し、コアがそれを切断として記録できるようにすること。
	bus.Unplug(board)
	waitFor(t, "切断", func() bool { return !h.src.Connected() })
	bus.Plug(board)

	h.expect("挿し直した後", allHigh, true)
}

func TestUnmonitoredPinsLoseTheirBit(t *testing.T) {
	board := hidpintest.NewBoard("SERIAL1")
	h := start(t, hidpintest.NewBus(board), Options{})

	h.expect("最初のイベント", allHigh, true)

	// 別のプログラムがピン設定を変え、GPIO0 を監視しなくなった。監視していないピンの
	// ビットは 0 になるので、基準を流し直すこと。
	config := board.PinConfig()
	config[0] = hidpin.Unused()
	if result := board.WritePinConfig(config); result != hidpin.ResultOK {
		t.Fatalf("ピン設定を書けません: %v", result)
	}

	h.expect("ピン設定の変更後", low(0), true)
}

func TestLostEdgeRebaselines(t *testing.T) {
	board := hidpintest.NewBoard("SERIAL1")
	h := start(t, hidpintest.NewBus(board), Options{})

	h.expect("最初のイベント", allHigh, true)

	// デバイスが変化を保持しきれずに捨てた。次の状態通知で値だけが変わって届く。
	board.LoseInput(2, false)
	board.Periodic()

	// 捨てられた変化はエッジにせず、今の値を基準にし直すこと。
	h.expect("取りこぼしの後", low(2), true)

	board.SetInput(2, true)
	h.expect("立上り", allHigh, false)
}

func TestMissedReportsRebaseline(t *testing.T) {
	board := hidpintest.NewBoard("SERIAL1")
	h := start(t, hidpintest.NewBus(board), Options{})

	h.expect("最初のイベント", allHigh, true)

	// 読み落としは状態通知どうしの並びの飛びで分かるので、まず 1 つ受け取らせる。
	board.Periodic()
	h.quiet()

	// ホストが状態通知を読み落とした。その間の変化は数え直せないので、変化を流す前に
	// 今の値で基準を取り直すこと。
	board.SkipReports(2)
	board.SetInput(3, false)

	h.expect("読み落としの後", allHigh, true)
	h.expect("立下り", low(3), false)
}

func TestSilentDeviceRestartsWatch(t *testing.T) {
	board := hidpintest.NewBoard("SERIAL1")
	// 定期通知が来なくなったことにする。USB は繋がったままなので、切断としては気づけない。
	h := start(t, hidpintest.NewBus(board), Options{silence: 100 * time.Millisecond})

	h.expect("最初のイベント", allHigh, true)

	// 監視をやり直し、開き直したところで基準を流し直すこと。
	h.expect("やり直しの後", allHigh, true)
	if got := board.OpenCount(); got != 1 {
		t.Errorf("開いているデバイスは %d 個, 期待は 1 個", got)
	}
}

func TestWaitsForTheDevice(t *testing.T) {
	board := hidpintest.NewBoard("SERIAL1")
	bus := hidpintest.NewBus()
	h := start(t, bus, Options{})

	// デバイスが無くても止まらず、挿したら読み始めること。
	h.quiet()
	if h.src.Connected() {
		t.Error("繋がっていないのに Connected() が true")
	}

	bus.Plug(board)
	h.expect("挿した後", allHigh, true)
}

func TestWarnsAboutUnmonitoredWiring(t *testing.T) {
	board := hidpintest.NewBoard("SERIAL1")
	// 偽のボードは Raspberry Pi Pico なので GPIO23 は無く、監視もしていない。そこに配線すると
	// ビットが 0 のままになり、アクティブローでは信号が出ているように見え続ける。
	wiring := config.Wiring{
		Bits:      map[signal.Role]int{signal.RoleStart: 0, signal.RoleBonus: 23},
		ActiveLow: true,
	}
	h := start(t, hidpintest.NewBus(board), Options{Wiring: wiring})

	h.expect("最初のイベント", allHigh, true)

	logs := h.logs.String()
	if !strings.Contains(logs, "bonus=23") {
		t.Errorf("監視外の配線を知らせていません:\n%s", logs)
	}
	if strings.Contains(logs, "start=0") {
		t.Errorf("監視しているピンまで知らせています:\n%s", logs)
	}
}

func TestEventTime(t *testing.T) {
	started := time.Now().Add(-time.Minute)
	st := &stream{started: started}

	// 変化が始まった時刻は、今との差で経過時間に直す。
	at, wall := st.eventTime(hidpin.OnOffChange{HasTime: true, Time: time.Now().Add(-20 * time.Millisecond)})
	if want := time.Minute - 20*time.Millisecond; at < want-time.Second || at > want+time.Second {
		t.Errorf("At = %v, 期待は %v あたり", at, want)
	}
	if wall.IsZero() {
		t.Error("Wall が空です")
	}

	// 時刻の分からない変化でも、時刻は後戻りさせない。
	at2, _ := st.eventTime(hidpin.OnOffChange{HasTime: true, Time: time.Now().Add(-time.Hour)})
	if at2 != at {
		t.Errorf("後戻りしました: %v → %v", at, at2)
	}
	at3, wall3 := st.eventTime(hidpin.OnOffChange{})
	if at3 < at2 {
		t.Errorf("後戻りしました: %v → %v", at2, at3)
	}
	if wall3.IsZero() {
		t.Error("Wall が空です")
	}
}
