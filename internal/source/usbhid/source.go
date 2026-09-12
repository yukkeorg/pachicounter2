// Package usbhid は USB-HID の GPIO デバイスを信号源にする。
//
// ポーリングはこのパッケージの内部に隠れる。信号源の契約はイベントストリーム型
// なので、ポーリングという概念は外に漏れない。詳細は
// docs/adr/0005-signal-source-abstraction.md を参照。
package usbhid

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/yukkeorg/pachicounter/internal/hid"
	"github.com/yukkeorg/pachicounter/internal/hidgpio"
	"github.com/yukkeorg/pachicounter/pkg/signal"
)

// DefaultInterval はポーリング間隔の既定値。
//
// 旧実装の 50ms（20Hz）から意図的に上げている。USB-IO 2.0 は 300Hz まで耐える仕様で、
// スタート信号のパルス幅は配線資料に記載が無いため、20ms 程度のパルスでも
// 取りこぼさない余裕を確保する。回転の取りこぼしは「たまに 1 回転ずれる」という
// 気づきにくい壊れ方をするので、速い側に倒す。
const DefaultInterval = 5 * time.Millisecond

// DefaultReconnectInterval はデバイスを探し直す間隔の既定値。
const DefaultReconnectInterval = 500 * time.Millisecond

// Options は信号源の設定。
type Options struct {
	// Driver はデバイスドライバの名前。空なら登録済みドライバ全てから自動検出する。
	Driver string

	// Interval はポーリング間隔。
	Interval time.Duration

	// ReadTimeout は 1 回の読み取りに許す時間。
	ReadTimeout time.Duration

	// ReconnectInterval はデバイスが見つからないときに探し直す間隔。
	ReconnectInterval time.Duration

	// Logger はログの出力先。nil なら slog の既定を使う。
	Logger *slog.Logger
}

func (o *Options) withDefaults() {
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.ReadTimeout <= 0 {
		// ポーリング間隔より長く取る。間隔と同じにすると、USB の遅延が少し
		// 揺れるだけでタイムアウト扱いになる。
		o.ReadTimeout = 10 * o.Interval
		if o.ReadTimeout < 20*time.Millisecond {
			o.ReadTimeout = 20 * time.Millisecond
		}
	}
	if o.ReconnectInterval <= 0 {
		o.ReconnectInterval = DefaultReconnectInterval
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// Source は USB-HID の GPIO デバイスを読む信号源。
//
// デバイスが繋がっていなくても動き出し、接続を待ち続ける。途中で抜けても止まらず、
// 再接続したら読み取りを再開する。配信中にコアが落ちるとセッションの数字を失うため、
// デバイスの不在で終了しない。詳細は
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
	label := "HID GPIO"
	if opts.Driver != "" {
		label = opts.Driver
	}
	s.name.Store(label)
	return s
}

// Name は信号源の名前を返す。接続後は実際に繋がった製品名になる。
func (s *Source) Name() string {
	if v, ok := s.name.Load().(string); ok {
		return v
	}
	return "HID GPIO"
}

// Connected は今この瞬間に台の信号を読めているかを返す。
func (s *Source) Connected() bool {
	return s.connected.Load()
}

// Events はポーリングを始めてイベントを流す。
func (s *Source) Events(ctx context.Context) (<-chan signal.Event, error) {
	out := make(chan signal.Event, 64)
	go s.run(ctx, out)
	return out, nil
}

func (s *Source) run(ctx context.Context, out chan<- signal.Event) {
	defer close(out)

	started := time.Now()
	log := s.opts.Logger

	for ctx.Err() == nil {
		conn, label, err := s.connect(ctx)
		if err != nil {
			// ctx が終わった場合だけ抜ける。それ以外は待ち続ける。
			if ctx.Err() != nil {
				return
			}
			continue
		}

		s.name.Store(label)
		s.connected.Store(true)
		log.Info("信号源に接続しました", "source", label)

		err = s.poll(ctx, conn, out, started)
		s.connected.Store(false)
		_ = conn.Close()

		if ctx.Err() != nil {
			return
		}
		log.Warn("信号源との接続が切れました。再接続を待ちます", "source", label, "err", err)
	}
}

// connect はデバイスが見つかるまで探し続ける。
func (s *Source) connect(ctx context.Context) (hidgpio.Conn, string, error) {
	ticker := time.NewTicker(s.opts.ReconnectInterval)
	defer ticker.Stop()

	var lastErr error
	logged := false

	for {
		found, err := hidgpio.Detect(s.opts.Driver)
		if err == nil {
			conn, openErr := found.Open(hidgpio.Config{ReadTimeout: s.opts.ReadTimeout})
			if openErr == nil {
				return conn, found.Driver.DisplayName, nil
			}
			err = openErr
		}

		if !errors.Is(err, hidgpio.ErrNoDevice) || !logged {
			s.opts.Logger.Info("信号源を待っています", "driver", s.opts.Driver, "err", err)
			logged = true
		}
		lastErr = err

		select {
		case <-ctx.Done():
			return nil, "", fmt.Errorf("接続を待つ間に終了しました: %w", lastErr)
		case <-ticker.C:
		}
	}
}

// poll はデバイスを一定間隔で読み、値が変わったときだけイベントを流す。
//
// 最初の 1 回は基準イベントとして必ず流す。受け手はこれをエッジとして扱わない。
// 起動時に立っているビットを立上りとして数えると、大当り中にコアを起動しただけで
// 大当り回数が増えてしまう。
func (s *Source) poll(ctx context.Context, conn hidgpio.Conn, out chan<- signal.Event, started time.Time) error {
	ticker := time.NewTicker(s.opts.Interval)
	defer ticker.Stop()

	var (
		prev      signal.Ports
		havePrev  bool
		timeouts  int
		maxErrors = 20
	)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}

		ports, err := conn.ReadPorts()
		if err != nil {
			if errors.Is(err, hid.ErrTimeout) {
				// 1 回のタイムアウトは珍しくない。続くなら接続が壊れている。
				timeouts++
				if timeouts < maxErrors {
					continue
				}
				return fmt.Errorf("読み取りが %d 回続けてタイムアウトしました: %w", timeouts, err)
			}
			return err
		}
		timeouts = 0

		baseline := !havePrev
		if havePrev && ports == prev {
			continue
		}
		prev = ports
		havePrev = true

		now := time.Now()
		ev := signal.Event{
			At:       now.Sub(started),
			Wall:     now,
			Ports:    ports,
			Baseline: baseline,
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- ev:
		}
	}
}
