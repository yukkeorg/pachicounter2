// Package app は信号源・集計・永続化・配信を結線する。
//
// 集計エンジンは並行に呼べないので、状態を触るのはこのパッケージの run ループだけに
// 限る。HTTP からの操作はコマンドとして run ループへ渡し、そこで順番に処理する。
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/yukkeorg/pachicounter2/api"
	"github.com/yukkeorg/pachicounter2/internal/config"
	"github.com/yukkeorg/pachicounter2/internal/counter"
	"github.com/yukkeorg/pachicounter2/internal/store"
	"github.com/yukkeorg/pachicounter2/pkg/machine"
	"github.com/yukkeorg/pachicounter2/pkg/signal"
)

// Options はアプリの設定。
type Options struct {
	// MachineID と Variant は集計する機種。
	MachineID string
	Variant   string

	// Wiring はビット位置と役割の対応。
	Wiring config.Wiring

	// Tuning は台の調整値。
	Tuning config.Tuning

	// Ops は運用パラメータ。
	Ops config.Ops

	// Source は信号源。
	Source signal.Source

	// Store は状態の保存先。Replay のときは渡さない。
	Store *store.Store

	// Replay が true なら、記録を流し直して確かめるための実行として扱い、保存先には
	// 何も書かない。続きのセッションにもつながない。再生元のログがすでに記録その
	// ものなので、写しを作ると実機のセッションのログと混ざる。詳細は
	// docs/adr/0006-raw-signal-log-plus-snapshot.md を参照。
	Replay bool

	// ForceNewSession が true なら、続けられるセッションがあっても新しく始める。
	ForceNewSession bool

	// Logger はログの出力先。
	Logger *slog.Logger
}

// ErrReplay は再生中には受け付けない操作であることを表す。再生は保存先に何も
// 書かないので、記録を変える操作（補正、新しいセッション）は意味を持たない。
var ErrReplay = errors.New("再生中は記録を変える操作を受け付けません")

// App はコアの本体。
type App struct {
	opts   Options
	log    *slog.Logger
	engine *counter.Engine

	writer       *store.Writer
	sessionID    string
	sessionStart time.Time

	// atOffset はセッション開始を 0 とする時刻に直すための下駄。信号源の時刻は
	// 信号源が動き出してからの経過時間なので、セッションの途中でコアを再起動すると
	// 0 に戻ってしまう。生信号ログの時刻はセッション内で単調に増えていなければ
	// 再集計できないため、ここで補正する。
	atOffset time.Duration

	cmds chan command

	hub    *hub
	latest struct {
		mu   sync.RWMutex
		snap api.Snapshot
	}
}

type command struct {
	kind    commandKind
	correct api.CorrectRequest
	note    string
	reply   chan error
}

type commandKind int

const (
	cmdNewSession commandKind = iota
	cmdCorrect
)

// New はアプリを組み立てる。機種プラグインを作り、配線を検証し、セッションを
// 決めるところまで行う。信号源はまだ読まない。
func New(opts Options) (*App, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Source == nil {
		return nil, errors.New("信号源が指定されていません")
	}
	switch {
	case opts.Replay && opts.Store != nil:
		return nil, errors.New("再生では保存先を使いません")
	case !opts.Replay && opts.Store == nil:
		return nil, errors.New("保存先が指定されていません")
	}

	plugin, err := machine.New(opts.MachineID, opts.Variant)
	if err != nil {
		return nil, err
	}

	engine, err := counter.New(plugin, opts.Wiring, opts.Tuning, opts.Ops)
	if err != nil {
		return nil, err
	}

	a := &App{
		opts:   opts,
		log:    opts.Logger,
		engine: engine,
		cmds:   make(chan command, 8),
		hub:    newHub(),
	}

	if opts.Replay {
		// 再生はどのセッションにも属さない。経過時間の起点だけを持つ。
		a.sessionStart = time.Now()
	} else if err := a.openSession(); err != nil {
		return nil, err
	}

	a.publish()
	return a, nil
}

// openSession は続けられるセッションがあれば続け、無ければ新しく始める。
func (a *App) openSession() error {
	if !a.opts.ForceNewSession {
		resumed, err := a.tryResume()
		if err != nil {
			a.log.Warn("セッションを続けられませんでした。新しく始めます", "err", err)
		} else if resumed {
			return nil
		}
	}
	return a.startSession()
}

// tryResume は直近のセッションを再集計して続ける。
//
// スナップショットを読むだけでは足りない。機種プラグインの内部状態（状態機械や
// 区間ごとの回転数）はスナップショットに現れないため、生信号ログを流し直さないと
// 復元できない。ログが唯一の真実である以上、復帰もログから行うのが筋である。
func (a *App) tryResume() (bool, error) {
	pointer, err := a.opts.Store.LoadPointer()
	if err != nil {
		if errors.Is(err, store.ErrNoSession) {
			return false, nil
		}
		return false, err
	}

	if pointer.Machine != a.opts.MachineID || pointer.Variant != a.opts.Variant {
		// 別機種の数字が同じセッションに混ざるのが最悪なので、機種が違えば
		// 続けない。詳細は docs/adr/0009-signal-baseline-and-device-loss.md を参照。
		a.log.Info("記録されている機種が違うため新しいセッションを始めます",
			"recorded", pointer.Machine, "requested", a.opts.MachineID)
		return false, nil
	}

	// 続けることは、過去のログを今の配線で読み直すことである。配線が違うと同じ
	// ポート値から別の信号を読み、過去の数字が別物になるので、機種が違うときと
	// 同じく続けない。
	start, err := a.opts.Store.SessionStart(pointer.Session)
	if err != nil {
		return false, err
	}
	recorded := config.Wiring{Bits: start.Wiring, ActiveLow: start.ActiveLow}
	if !recorded.Equal(a.opts.Wiring) {
		a.log.Info("記録されている配線が違うため新しいセッションを始めます",
			"recorded", recorded.String(), "requested", a.opts.Wiring.String())
		return false, nil
	}

	var (
		lastAt   time.Duration
		lastWall time.Time
		replayed int
	)

	err = a.opts.Store.ReadSession(pointer.Session, func(rec store.Record) error {
		replayed++
		if rec.At > lastAt {
			lastAt = rec.At
		}
		if !rec.Wall.IsZero() {
			lastWall = rec.Wall
		}

		switch rec.Kind {
		case store.KindSessionStart:
			a.sessionStart = rec.Wall

		case store.KindBaseline:
			if rec.Ports != nil {
				a.engine.Baseline(signal.Event{At: rec.At, Wall: rec.Wall, Ports: *rec.Ports, Baseline: true})
			}

		case store.KindSignal:
			if rec.Ports != nil {
				a.engine.Apply(signal.Event{At: rec.At, Wall: rec.Wall, Ports: *rec.Ports})
			}

		case store.KindCorrect:
			if err := a.engine.Correct(rec.Counter, rec.Delta); err != nil {
				// 記録済みの補正が今の規則で通らないことはありうる。止めるほどでは
				// ないので、記録に残して読み飛ばす。
				a.log.Warn("記録されている補正を適用できませんでした", "counter", rec.Counter, "delta", rec.Delta, "err", err)
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}

	writer, err := a.opts.Store.AppendSession(pointer.Session)
	if err != nil {
		return false, err
	}

	a.writer = writer
	a.sessionID = pointer.Session
	if a.sessionStart.IsZero() {
		a.sessionStart = time.Now()
	}

	// ログの続きが単調に増えるように下駄を決める。前回の最後の記録から今までの
	// 実時間ぶんを足す。実時刻が取れなければ、少なくとも戻らないようにする。
	a.atOffset = lastAt
	if !lastWall.IsZero() {
		if gap := time.Since(lastWall); gap > 0 {
			a.atOffset += gap
		}
	}

	a.log.Info("セッションを続けます",
		"session", a.sessionID,
		"records", replayed,
		"rotations", a.engine.Counters().TotalRotations(),
		"bonuses", a.engine.Counters().Bonuses)
	return true, nil
}

// startSession は新しいセッションを始める。
func (a *App) startSession() error {
	now := time.Now()
	id := store.NewSessionID(now, a.opts.MachineID)

	writer, err := a.opts.Store.CreateSession(id)
	if err != nil {
		return err
	}

	a.engine.Reset()
	a.writer = writer
	// 同じ秒に続けて始めた場合、実際に使われた識別子は要求したものと違う。
	a.sessionID = writer.ID()
	a.sessionStart = now
	a.atOffset = 0

	// 1 行目に機種と配線を書く。後から再集計するとき、当時の配線が分からないと
	// ポート値を解釈できない。
	rec := store.Record{
		Kind:      store.KindSessionStart,
		At:        0,
		Wall:      now,
		Session:   a.sessionID,
		Machine:   a.opts.MachineID,
		Variant:   a.opts.Variant,
		Wiring:    a.opts.Wiring.Bits,
		ActiveLow: a.opts.Wiring.ActiveLow,
	}
	if err := a.writer.Append(rec); err != nil {
		return err
	}

	a.log.Info("新しいセッションを始めました", "session", a.sessionID, "machine", a.opts.MachineID)
	return nil
}

// Close はセッションのログを閉じる。
func (a *App) Close() error {
	if a.writer == nil {
		return nil
	}
	return a.writer.Close()
}

// SessionID は今のセッション識別子を返す。
func (a *App) SessionID() string { return a.sessionID }

// Run は信号源を読み始め、ctx が終わるまで集計を続ける。
func (a *App) Run(ctx context.Context) error {
	events, err := a.opts.Source.Events(ctx)
	if err != nil {
		return err
	}

	// デバウンスで見送ったエッジを拾い直すための定期処理。信号源は値が変わった
	// ときだけイベントを流すので、これが無いとロックアウトが明けた状態に
	// 追いつけない。
	tickInterval := a.opts.Ops.Debounce
	if tickInterval <= 0 {
		tickInterval = 10 * time.Millisecond
	}
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	// 信号源の接続状態を見張る。デバイスが抜けてもコアは止まらないが、
	// フロントには「今読めていない」ことを伝える。
	watch := time.NewTicker(500 * time.Millisecond)
	defer watch.Stop()

	var (
		atRef     time.Duration
		wallRef   = time.Now()
		haveRef   bool
		wasOnline = a.opts.Source.Connected()

		// baselines はこの Run で受け取った基準イベントの数。2 回目以降は再接続を
		// 意味する。開始時の Connected() で判断すると、常に true を返す信号源や、
		// 読み始める前に繋がった信号源で、最初の基準イベントを再接続と取り違える。
		baselines int
	)

	for {
		select {
		case <-ctx.Done():
			return nil

		case ev, ok := <-events:
			if !ok {
				a.log.Info("信号源が終了しました")
				return nil
			}

			ev.At += a.atOffset
			atRef, wallRef, haveRef = ev.At, time.Now(), true

			if ev.Baseline {
				if ev.Restart && !a.opts.Replay {
					// 記録しているセッションで集計を黙ってやり直すと、ログを再集計しても
					// 同じ数字にならない。やり直しは受け付けない。
					a.log.Warn("記録しているセッションでは信号源のやり直しを受け付けません",
						"source", a.opts.Source.Name())
				}

				switch {
				case ev.Restart && a.opts.Replay:
					// 記録を先頭から流し直した。再接続と違い、集計を初めからやり直す。
					a.engine.Reset()
				case baselines > 0:
					// 再接続。切れている間の変化は取り逃しているので、記録にもそれを残す。
					a.append(store.Record{Kind: store.KindDisconnect, At: ev.At, Wall: ev.Wall})
				}
				baselines++

				a.engine.Baseline(ev)
				a.append(store.Record{Kind: store.KindBaseline, At: ev.At, Wall: ev.Wall, Ports: &ev.Ports})
				a.publish()
				continue
			}

			a.append(store.Record{Kind: store.KindSignal, At: ev.At, Wall: ev.Wall, Ports: &ev.Ports})
			if a.engine.Apply(ev) {
				a.publish()
			}

		case <-ticker.C:
			if !a.engine.Pending() || !haveRef {
				continue
			}
			at := atRef + time.Since(wallRef)
			if a.engine.Tick(at, time.Now()) {
				a.publish()
			}

		case <-watch.C:
			online := a.opts.Source.Connected()
			if online != wasOnline {
				wasOnline = online
				a.publish()
			}

		case cmd := <-a.cmds:
			cmd.reply <- a.handle(cmd)
		}
	}
}

func (a *App) handle(cmd command) error {
	switch cmd.kind {
	case cmdNewSession:
		if err := a.writer.Close(); err != nil {
			return err
		}
		if err := a.startSession(); err != nil {
			return err
		}
		a.publish()
		return nil

	case cmdCorrect:
		at := a.engine.Elapsed()
		if err := a.engine.Correct(cmd.correct.Counter, cmd.correct.Delta); err != nil {
			return err
		}

		// 補正は集計結果を直接書き換えるのではなく、ログに残して再集計でも
		// 再現させる。詳細は docs/adr/0008-corrections-are-log-events.md を参照。
		a.append(store.Record{
			Kind:    store.KindCorrect,
			At:      at,
			Wall:    time.Now(),
			Counter: cmd.correct.Counter,
			Delta:   cmd.correct.Delta,
			Note:    cmd.correct.Note,
		})
		a.publish()
		return nil

	default:
		return fmt.Errorf("知らない操作です")
	}
}

func (a *App) append(rec store.Record) {
	// 再生のときは何も書かない。
	if a.opts.Replay || a.writer == nil {
		return
	}
	if err := a.writer.Append(rec); err != nil {
		// 記録に失敗しても集計と表示は続ける。配信中に落ちるより、記録を
		// 欠いたまま動き続ける方がましである。
		a.log.Error("生信号ログに書けませんでした", "err", err)
	}
}

// publish はスナップショットを組み立てて保存し、フロントへ配る。再生のときは保存しない。
func (a *App) publish() {
	snap := a.engine.Snapshot()
	snap.Session = api.SessionInfo{
		ID:         a.sessionID,
		StartedAt:  a.sessionStart,
		ElapsedSec: a.engine.Elapsed().Seconds(),
	}
	snap.Device = api.DeviceInfo{
		Source:    a.opts.Source.Name(),
		Connected: a.opts.Source.Connected(),
	}

	a.latest.mu.Lock()
	a.latest.snap = snap
	a.latest.mu.Unlock()

	if !a.opts.Replay {
		if err := a.opts.Store.SaveSnapshot(store.Pointer{
			Session: a.sessionID,
			Machine: a.opts.MachineID,
			Variant: a.opts.Variant,
		}, snap); err != nil {
			a.log.Error("スナップショットを保存できませんでした", "err", err)
		}
	}

	a.hub.broadcast(snap)
}

// Snapshot は最新のスナップショットを返す。
func (a *App) Snapshot() api.Snapshot {
	a.latest.mu.RLock()
	defer a.latest.mu.RUnlock()
	return a.latest.snap
}

// Subscribe はスナップショットの流れを受け取る。戻り値の関数で購読をやめる。
func (a *App) Subscribe() (<-chan api.Snapshot, func()) {
	return a.hub.subscribe()
}

// NewSession は新しいセッションを始める。再生中は ErrReplay を返す。
func (a *App) NewSession(note string) error {
	if a.opts.Replay {
		return fmt.Errorf("新しいセッションを始められません: %w", ErrReplay)
	}
	return a.send(command{kind: cmdNewSession, note: note})
}

// Correct はカウンタを手動補正する。再生中は ErrReplay を返す。
func (a *App) Correct(req api.CorrectRequest) error {
	if a.opts.Replay {
		return fmt.Errorf("補正できません: %w", ErrReplay)
	}
	return a.send(command{kind: cmdCorrect, correct: req})
}

func (a *App) send(cmd command) error {
	cmd.reply = make(chan error, 1)

	select {
	case a.cmds <- cmd:
	case <-time.After(2 * time.Second):
		return errors.New("コアが操作を受け付けませんでした")
	}

	select {
	case err := <-cmd.reply:
		return err
	case <-time.After(5 * time.Second):
		return errors.New("操作の結果を受け取れませんでした")
	}
}
