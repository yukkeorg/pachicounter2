// Package hidpin は hidpin（RP2040 で作った USB-HID の GPIO デバイス）を信号源にする。
//
// usbhid と違ってポーリングしない。hidpin はピンの変化をチャタリング除去してから、変化が
// 始まった時刻をつけて送ってくる（push 型）。デバイスを待つこと、抜き挿しへの追従、
// デバイスの時計をホストの時計に直すこと、取りこぼしの突き合わせは、hidpin の Go ライブラリ
// の Watcher が受け持つ。このパッケージはその知らせを、コアが読む生のポート値のイベントに直す。
//
// ポート値のビット n は GPIO n に当たる。アクティブローかどうかの解釈はコア（-active-low）の
// 仕事なので、Watcher には「どのピンもアクティブハイ」と伝え、ON/OFF ではなくピンレベルを
// そのまま受け取る。
//
// このパッケージも名前が hidpin なので、ソース中の hidpin. はライブラリを指す。
// プロトコルは hidpin リポジトリの docs/PROTOCOL.md を参照。
package hidpin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yukkeorg/hidpin/host-go/hidpin"
	"github.com/yukkeorg/pachicounter2/internal/config"
	"github.com/yukkeorg/pachicounter2/pkg/signal"
)

// DefaultScanInterval はデバイスを探し直す間隔の既定値。監視をやり直すときの待ち時間でもある。
const DefaultScanInterval = hidpin.DefaultScanInterval

// baselineDelay は基準イベントを流すまでに待つ時間。1 つの状態通知から出る知らせはまとめて
// 届くので、それが途切れてから 1 つだけ流す。1 つずつ流すと、同じ状態通知から基準イベントが
// いくつも出る。
const baselineDelay = 50 * time.Millisecond

// silentPeriods は、定期通知の間隔いくつ分のあいだ状態通知が届かなかったら監視をやり直すか。
// hidpin は変化が無くても定期的に状態通知を送ってくるので、届かないのは異常である。
// USB が繋がったままデバイスが黙る場合、Watcher は切断として気づけない。
const silentPeriods = 3

// Options は信号源の設定。
type Options struct {
	// Serial は読むデバイスのシリアル番号。空なら、最初に見つけた 1 台を読む。
	Serial string

	// ScanInterval はデバイスを探し直す間隔。0 なら DefaultScanInterval。
	ScanInterval time.Duration

	// Logger はログの出力先。nil なら slog の既定を使う。
	Logger *slog.Logger

	// Wiring は指定された配線。配線したビットをデバイスが監視しているかを確かめるために
	// 使うだけで、信号の解釈には使わない。
	Wiring config.Wiring

	// bus はデバイスを探して開く経路。nil なら OS のもの。テストで差し替える。
	bus hidpin.Bus

	// silence は状態通知が途切れたと判断するまでの時間。0 ならデバイスが知らせる定期通知の
	// 間隔から決める。テストで差し替える。
	silence time.Duration
}

func (o *Options) withDefaults() {
	if o.ScanInterval <= 0 {
		o.ScanInterval = DefaultScanInterval
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// Source は hidpin を読む信号源。
//
// usbhid と同じく、デバイスが繋がっていなくても動き出し、接続を待ち続ける。途中で抜けても
// 止まらず、再接続したら読み取りを再開する。詳細は
// docs/adr/0009-signal-baseline-and-device-loss.md を参照。
type Source struct {
	opts      Options
	connected atomic.Bool
	name      atomic.Value // string
}

// New は信号源を作る。この時点ではデバイスを探さない。
func New(opts Options) *Source {
	opts.withDefaults()

	s := &Source{opts: opts}
	s.name.Store("hidpin")
	return s
}

// Name は信号源の名前を返す。接続後はボードの名前が入る。
func (s *Source) Name() string {
	if v, ok := s.name.Load().(string); ok {
		return v
	}
	return "hidpin"
}

// Connected は今この瞬間に台の信号を読めているかを返す。
func (s *Source) Connected() bool {
	return s.connected.Load()
}

// Events は監視を始めてイベントを流す。
func (s *Source) Events(ctx context.Context) (<-chan signal.Event, error) {
	out := make(chan signal.Event, 64)
	go s.run(ctx, out)
	return out, nil
}

func (s *Source) run(ctx context.Context, out chan<- signal.Event) {
	defer close(out)

	st := &stream{src: s, log: s.opts.Logger, out: out, started: time.Now()}

	for ctx.Err() == nil {
		w, err := hidpin.Watch(ctx, hidpin.WatchOptions{
			Serial:       s.opts.Serial,
			ScanInterval: s.opts.ScanInterval,
			ActiveLow:    activeHigh(),
			Bus:          s.opts.bus,
		})
		if err != nil {
			s.opts.Logger.Error("hidpin の監視を始められません", "err", err)
			if !wait(ctx, s.opts.ScanInterval) {
				return
			}
			continue
		}

		err = st.pump(ctx, w)
		s.connected.Store(false)
		_ = w.Close()
		if ctx.Err() != nil {
			return
		}
		if stopped := w.Err(); stopped != nil {
			err = stopped
		}

		// 監視が終わる理由は、デバイスが 2 台あって選べない、この OS では使えない、
		// デバイスが黙ったまま、のいずれか。どれも待てば直りうるので、やり直す。
		s.opts.Logger.Warn("hidpin の監視をやり直します", "err", err)
		if !wait(ctx, s.opts.ScanInterval) {
			return
		}
	}
}

// activeHigh は「どのピンもアクティブハイ」という指定。こうすると Watcher の ON/OFF が
// ピンレベルそのものになり、アクティブローの解釈はコアだけが持つ。
func activeHigh() map[int]bool {
	polarity := make(map[int]bool, hidpin.GPIOCount)
	for gpio := range hidpin.GPIOCount {
		polarity[gpio] = false
	}
	return polarity
}

func wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// stream は Watcher の知らせをイベントに直す途中の状態。
type stream struct {
	src     *Source
	log     *slog.Logger
	out     chan<- signal.Event
	started time.Time

	// ports は手元が知っているピンレベル。監視していないピンのビットは 0 になる。
	ports     signal.Ports
	monitored signal.Ports
	known     bool // ピンレベルを 1 度でも受け取ったか
	lastAt    time.Duration

	// pending は基準イベントを流す番が来ていることを表す。
	pending bool

	silence    time.Duration
	lastReport time.Time
}

// pump は Watcher の知らせを受け取り続ける。戻るのは監視が終わったときか、状態通知が
// 途切れたとき。
func (st *stream) pump(ctx context.Context, w *hidpin.Watcher) error {
	check := time.NewTicker(500 * time.Millisecond)
	defer check.Stop()

	for {
		// 基準イベントは、知らせが途切れてから 1 つだけ流す。
		var flush <-chan time.Time
		if st.pending && st.known {
			flush = time.After(baselineDelay)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-flush:
			if err := st.baseline(ctx); err != nil {
				return err
			}

		case <-check.C:
			if st.silence > 0 && time.Since(st.lastReport) > st.silence {
				return fmt.Errorf("状態通知が %s 届きません", st.silence)
			}

		case ev, ok := <-w.Events():
			if !ok {
				return errors.New("監視が終わりました")
			}
			if err := st.handle(ctx, ev); err != nil {
				return err
			}
		}
	}
}

func (st *stream) handle(ctx context.Context, ev hidpin.Event) error {
	switch e := ev.(type) {
	case hidpin.Connected:
		st.setConfig(e.PinConfig)
		name := fmt.Sprintf("hidpin (%s)", e.Info.BoardName())
		st.src.name.Store(name)
		st.src.connected.Store(true)
		st.silence = st.src.opts.silence
		if st.silence <= 0 {
			st.silence = silentPeriods * periodicInterval(e.Info)
		}
		st.lastReport = time.Now()
		st.pending = true
		st.log.Info("信号源に接続しました", "source", name, "serial", e.Serial,
			"firmware", e.Info.FirmwareVersion(), "monitored", gpioList(e.PinConfig.MonitoredMask()))
		st.warnUnmonitored()

	case hidpin.Disconnected:
		st.src.connected.Store(false)
		st.silence = 0
		st.log.Warn("信号源との接続が切れました。再接続を待ちます", "err", e.Err)

	case hidpin.ConnectFailed:
		st.log.Warn("hidpin を開けません", "err", e.Err)

	case hidpin.StatusReceived:
		st.lastReport = time.Now()
		if e.Missed > 0 {
			// 読み落とした状態通知にあった変化は数え直せない。
			st.log.Warn("状態通知を読み落としました。今の値を基準にし直します", "count", e.Missed)
			st.pending = true
		}

	case hidpin.InitialOnOff:
		// 監視し始めたピンのピンレベル。エッジではないので基準にする。
		for gpio, level := range e.On {
			st.setLevel(gpio, level)
		}
		st.pending = true

	case hidpin.OnOffChange:
		if e.Inferred {
			// 変化の知らせが無いまま値が変わっていた。デバイスが変化を捨てたか、切れている
			// 間に変わったか。取りこぼした変化は数え直せないので、今の値を基準にし直す。
			st.log.Warn("変化を取りこぼしました。今の値を基準にし直します", "gpio", e.GPIO, "level", e.Level)
			st.setLevel(e.GPIO, e.Level)
			st.pending = true
			return nil
		}
		// 溜めている基準は、変化より先に流す。
		if err := st.baseline(ctx); err != nil {
			return err
		}
		st.setLevel(e.GPIO, e.Level)
		at, wall := st.eventTime(e)
		return st.emit(ctx, signal.Event{At: at, Wall: wall, Ports: st.ports})

	case hidpin.PinConfigChanged:
		// 別のプログラムがピン設定を変えた。監視するピンが変われば、ポート値の意味も変わる。
		st.log.Warn("ピン設定が変わりました", "monitored", gpioList(e.Config.MonitoredMask()))
		st.setConfig(e.Config)
		st.warnUnmonitored()
		st.pending = true
	}
	return nil
}

// baseline は溜めている基準イベントを流す。
func (st *stream) baseline(ctx context.Context) error {
	if !st.pending || !st.known {
		return nil
	}
	st.pending = false

	now := time.Now()
	return st.emit(ctx, signal.Event{
		At:       st.clamp(now.Sub(st.started)),
		Wall:     now,
		Ports:    st.ports,
		Baseline: true,
	})
}

func (st *stream) emit(ctx context.Context, ev signal.Event) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case st.out <- ev:
		return nil
	}
}

// eventTime は変化の時刻を、経過時間と実時刻に直す。
//
// Watcher が返す時刻は実時刻で、単調増加クロックを持たない。そのまま引き算すると時刻合わせ
// （NTP）の影響を受けるので、今との差だけを使い、経過時間そのものは単調増加クロックから取る。
func (st *stream) eventTime(e hidpin.OnOffChange) (time.Duration, time.Time) {
	now := time.Now()
	if !e.HasTime {
		// 変化が始まった時刻が分からない（デバイスの中で長く待たされた変化）。
		return st.clamp(now.Sub(st.started)), now
	}
	ago := max(now.Round(0).Sub(e.Time), 0)
	return st.clamp(now.Sub(st.started) - ago), e.Time
}

// clamp は時刻を後戻りさせない。変化の時刻はデバイスの時計から出すので、ホストの時計との
// ずれの直し方によっては前のイベントより前になりうる。
func (st *stream) clamp(at time.Duration) time.Duration {
	st.lastAt = max(at, st.lastAt)
	return st.lastAt
}

func (st *stream) setLevel(gpio int, high bool) {
	if gpio < 0 || gpio >= signal.PortBits {
		return
	}
	bit := signal.Ports(1) << gpio
	if high {
		st.ports |= bit
	} else {
		st.ports &^= bit
	}
	st.known = true
}

// warnUnmonitored は、配線したビットに当たる GPIO をデバイスが監視していなければ警告する。
//
// 監視していないピンのビットは 0 になるので、アクティブローの配線では信号が出ているように
// 見え続ける。回転も大当りも数えられないまま静かに壊れるので、気づけるようにする。
func (st *stream) warnUnmonitored() {
	wiring := st.src.opts.Wiring

	var unmonitored []string
	for _, role := range signal.AllRoles() {
		bit, wired := wiring.Bits[role]
		if !wired || st.monitored.Bit(bit) {
			continue
		}
		unmonitored = append(unmonitored, fmt.Sprintf("%s=%d", role, bit))
	}
	if len(unmonitored) == 0 {
		return
	}

	st.log.Warn("配線したビットに当たる GPIO を hidpin が監視していません。その信号は読めません",
		"wiring", strings.Join(unmonitored, ","), "monitored", gpioList(uint32(st.monitored)))
}

// setConfig は監視しているピンを覚え、監視していないピンのビットを落とす。
func (st *stream) setConfig(config hidpin.PinConfig) {
	st.monitored = signal.Ports(config.MonitoredMask())
	st.ports &= st.monitored
}

func periodicInterval(info hidpin.DeviceInfo) time.Duration {
	if info.PeriodicIntervalMS == 0 {
		return time.Second
	}
	return time.Duration(info.PeriodicIntervalMS) * time.Millisecond
}

func gpioList(mask uint32) string {
	gpios := hidpin.MaskToGPIOs(mask)
	if len(gpios) == 0 {
		return "なし"
	}
	parts := make([]string, len(gpios))
	for i, gpio := range gpios {
		parts[i] = strconv.Itoa(gpio)
	}
	return strings.Join(parts, ",")
}
