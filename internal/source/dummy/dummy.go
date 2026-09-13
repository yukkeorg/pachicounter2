// Package dummy はハードウェアを持たない信号源を提供する。
//
// 手で書いた信号列を流せるので、集計ロジックの検証に使う。機種の挙動は実機で
// 録った生信号ログの再生で検証する（internal/source/replay）。
package dummy

import (
	"context"
	"time"

	"github.com/yukkeorg/pachicounter2/pkg/signal"
)

// Step は流すイベント 1 つ分の指示。
type Step struct {
	// After は 1 つ前のイベントからの待ち時間。
	After time.Duration

	// Ports は変化後のポート値。
	Ports signal.Ports

	// Baseline が true なら基準イベントとして流す。再接続を試すときに使う。
	// 最初のイベントは指定が無くても基準イベントになる。
	Baseline bool
}

// Source は与えられた信号列をそのまま流す信号源。
type Source struct {
	steps []Step

	// Realtime が true なら Step.After の時間だけ実際に待つ。false なら待たずに
	// 流し、イベントの時刻だけを After の累積で進める。テストでは false にする。
	Realtime bool
}

// New は信号列を流す信号源を作る。
func New(steps []Step, realtime bool) *Source {
	return &Source{steps: steps, Realtime: realtime}
}

// Name は信号源の名前を返す。
func (s *Source) Name() string { return "ダミー" }

// Connected は常に true を返す。ダミーは抜けることがない。
func (s *Source) Connected() bool { return true }

// Events は信号列を流す。流し終わってもチャネルは閉じず、ctx の終了まで開いたままに
// する。閉じてしまうとコアが「信号源が終わった」と判断して止まるため、手元で
// 見ながら試すときに都合が悪い。
func (s *Source) Events(ctx context.Context) (<-chan signal.Event, error) {
	out := make(chan signal.Event, 16)

	go func() {
		defer close(out)

		started := time.Now()
		var elapsed time.Duration

		for i, step := range s.steps {
			if s.Realtime && step.After > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(step.After):
				}
			}
			elapsed += step.After

			ev := signal.Event{
				At:       elapsed,
				Wall:     started.Add(elapsed),
				Ports:    step.Ports,
				Baseline: i == 0 || step.Baseline,
			}

			select {
			case <-ctx.Done():
				return
			case out <- ev:
			}
		}

		<-ctx.Done()
	}()

	return out, nil
}
