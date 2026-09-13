// Package replay は記録した生信号ログを信号源として流す。
//
// 再生を専用コマンドではなく信号源の 1 実装として持つので、コア本体を
// --source file:... で起動すれば、集計・スナップショット・SSE・フロントまで
// 通しで実機なしに検証できる。詳細は
// docs/adr/0005-signal-source-abstraction.md を参照。
package replay

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/yukkeorg/pachicounter2/internal/config"
	"github.com/yukkeorg/pachicounter2/internal/source"
	"github.com/yukkeorg/pachicounter2/internal/store"
	"github.com/yukkeorg/pachicounter2/pkg/signal"
)

// file と loop は同じ実装を使うが、1 回流して止まるか繰り返すかが違うので、
// 別々に登録する。
func init() {
	source.Register(source.Registration{
		Name:    "file",
		Summary: "記録した生信号ログを 1 回流す。保存先には何も書かない",
		Usage:   "file:<生信号ログのパス>",
		Kind:    source.KindReplay,
		New: func(arg string, _ source.Env) (signal.Source, error) {
			src, err := New(Options{Path: arg, Realtime: true, Speed: 1})
			if err != nil {
				return nil, err
			}
			return src, nil
		},
	})

	source.Register(source.Registration{
		Name:    "loop",
		Summary: "記録した生信号ログを先頭から繰り返し流す。保存先には何も書かない",
		Usage:   "loop:<生信号ログのパス>",
		Kind:    source.KindReplay,
		New: func(arg string, _ source.Env) (signal.Source, error) {
			src, err := New(Options{Path: arg, Realtime: true, Speed: 1, Loop: true})
			if err != nil {
				return nil, err
			}
			return src, nil
		},
	})
}

// Options は再生の設定。
type Options struct {
	// Path は読み込む JSONL のパス。
	Path string

	// Realtime が true なら記録された間隔どおりに待って流す。false なら
	// 待たずに一気に流す（テストと再集計はこちら）。
	Realtime bool

	// Speed は Realtime のときの再生速度。1 で等速、2 で 2 倍速。
	Speed float64

	// Loop が true なら流し終わったら先頭から繰り返す。フロントの見た目を
	// 調整するときに、同じ流れを何度も見られる。
	Loop bool
}

// Source は生信号ログを流す信号源。
type Source struct {
	opts Options
}

// New は再生する信号源を作る。
func New(opts Options) (*Source, error) {
	if opts.Path == "" {
		return nil, fmt.Errorf("再生するログのパスが指定されていません")
	}
	if _, err := os.Stat(opts.Path); err != nil {
		return nil, fmt.Errorf("再生するログ %s を読めません: %w", opts.Path, err)
	}
	if opts.Speed <= 0 {
		opts.Speed = 1
	}
	return &Source{opts: opts}, nil
}

// Name は信号源の名前を返す。
func (s *Source) Name() string { return "再生: " + s.opts.Path }

// Connected は常に true を返す。
func (s *Source) Connected() bool { return true }

// Recorded は記録したときの機種と配線を、ログの 1 行目の session_start から読む。
func (s *Source) Recorded() (source.Recorded, error) {
	rec, err := store.ReadSessionStart(s.opts.Path)
	if err != nil {
		return source.Recorded{}, err
	}
	return source.Recorded{
		Machine: rec.Machine,
		Variant: rec.Variant,
		Wiring:  config.Wiring{Bits: rec.Wiring, ActiveLow: rec.ActiveLow},
	}, nil
}

// Events はログを読んでイベントを流す。
//
// ポート値を持つレコード（基準イベントと信号）だけを流す。補正レコードは
// 信号ではないので、ここでは流さない。記録済みの補正を含めて再集計したい場合は、
// 再集計の経路でログを直接読む。
func (s *Source) Events(ctx context.Context) (<-chan signal.Event, error) {
	out := make(chan signal.Event, 64)

	go func() {
		defer close(out)

		for {
			if err := s.play(ctx, out); err != nil {
				return
			}
			if !s.opts.Loop || ctx.Err() != nil {
				break
			}
		}

		// 流し終わってもチャネルは閉じず、ctx の終了まで開いたままにする。
		// 閉じるとコアが「信号源が終わった」と判断して止まってしまう。
		<-ctx.Done()
	}()

	return out, nil
}

func (s *Source) play(ctx context.Context, out chan<- signal.Event) error {
	f, err := os.Open(s.opts.Path)
	if err != nil {
		return err
	}
	defer f.Close()

	started := time.Now()
	var prevAt time.Duration
	first := true

	return store.ReadRecords(f, func(rec store.Record) error {
		if rec.Ports == nil {
			return nil
		}

		if s.opts.Realtime && !first {
			wait := rec.At - prevAt
			if s.opts.Speed != 1 {
				wait = time.Duration(float64(wait) / s.opts.Speed)
			}
			if wait > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(wait):
				}
			}
		}
		prevAt = rec.At
		first = false

		ev := signal.Event{
			At:       rec.At,
			Wall:     rec.Wall,
			Ports:    *rec.Ports,
			Baseline: rec.Kind == store.KindBaseline,
		}
		if ev.Wall.IsZero() {
			ev.Wall = started.Add(rec.At)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- ev:
		}
		return nil
	})
}
