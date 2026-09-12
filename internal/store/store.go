// Package store はセッションの生信号ログと集計スナップショットを永続化する。
//
// 生信号ログが唯一の真実で、スナップショットはその導出物である。セッションの
// 途中でコアを再起動したときは、スナップショットを読むのではなくログを再集計して
// 復帰する。機種プラグインの内部状態（状態機械や経過時間）はスナップショットに
// 現れないため、ログを流し直さないと復元できない。詳細は
// docs/adr/0006-raw-signal-log-plus-snapshot.md を参照。
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ErrNoSession は続けられるセッションが無いことを表す。
var ErrNoSession = errors.New("store: 続けられるセッションがありません")

// Store は状態の保存先。
type Store struct {
	dir string
}

// DefaultDir は状態の保存先を返す。
//
// 旧実装は ~/.config/pcounter.d に置いていたが、設定用ディレクトリに状態を
// 置いていたことになる。XDG に従って状態用のディレクトリを使う。
func DefaultDir() (string, error) {
	if runtime.GOOS == "windows" {
		if dir := os.Getenv("LocalAppData"); dir != "" {
			return filepath.Join(dir, "pachicounter"), nil
		}
	}

	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "pachicounter"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("ホームディレクトリが分かりません: %w", err)
	}
	return filepath.Join(home, ".local", "state", "pachicounter"), nil
}

// Open は保存先を用意して Store を返す。
func Open(dir string) (*Store, error) {
	if dir == "" {
		d, err := DefaultDir()
		if err != nil {
			return nil, err
		}
		dir = d
	}

	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o755); err != nil {
		return nil, fmt.Errorf("保存先 %s を作れません: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// Dir は保存先を返す。
func (s *Store) Dir() string { return s.dir }

func (s *Store) sessionPath(id string) string {
	return filepath.Join(s.dir, "sessions", id+".jsonl")
}

func (s *Store) snapshotPath() string {
	return filepath.Join(s.dir, "snapshot.json")
}

// NewSessionID はセッション識別子を作る。実時刻と機種から組み立てるので、
// ファイル名を見ただけでいつ何を打ったセッションか分かる。
func NewSessionID(now time.Time, machineID string) string {
	return fmt.Sprintf("%s-%s", now.Format("20060102-150405"), machineID)
}

// Writer はセッションの生信号ログに追記する。
type Writer struct {
	mu sync.Mutex
	f  *os.File
	w  *bufio.Writer
	id string
}

// CreateSession は新しいセッションのログを作って追記用に開く。
//
// 識別子は秒単位の実時刻から作るので、同じ秒のうちに続けてセッションを始めると
// ぶつかる。既存のログを上書きすると記録が消えるため、空いている名前を探して
// 使う。実際に使った識別子は Writer.ID() で分かる。
func (s *Store) CreateSession(id string) (*Writer, error) {
	for attempt := 1; attempt <= 100; attempt++ {
		candidate := id
		if attempt > 1 {
			candidate = fmt.Sprintf("%s-%d", id, attempt)
		}

		path := s.sessionPath(candidate)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("セッションのログ %s を作れません: %w", path, err)
		}
		return &Writer{f: f, w: bufio.NewWriter(f), id: candidate}, nil
	}
	return nil, fmt.Errorf("セッションのログの空き名前を見つけられません: %s", id)
}

// AppendSession は既にあるセッションのログを追記用に開く。
func (s *Store) AppendSession(id string) (*Writer, error) {
	path := s.sessionPath(id)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("セッションのログ %s を開けません: %w", path, err)
	}
	return &Writer{f: f, w: bufio.NewWriter(f), id: id}, nil
}

// ID は書き込み先のセッション識別子を返す。
func (w *Writer) ID() string { return w.id }

// Append は 1 レコード追記して即座に書き出す。
//
// バッファに溜めない。書き込みは回転ごとで毎秒数行にすぎず、溜める利点が無い一方、
// 電源断で溜めた分が消えるのは ADR-0006 で避けたかったことそのものである。
func (w *Writer) Append(r Record) error {
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("レコードを JSON にできません: %w", err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.f == nil {
		return os.ErrClosed
	}
	if _, err := w.w.Write(line); err != nil {
		return err
	}
	if err := w.w.WriteByte('\n'); err != nil {
		return err
	}
	return w.w.Flush()
}

// Close はログを閉じる。
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.f == nil {
		return nil
	}
	f := w.f
	w.f = nil

	if err := w.w.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// ReadSession はセッションのログを読んで 1 レコードずつ fn に渡す。
// fn がエラーを返したらそこで止める。
func (s *Store) ReadSession(id string, fn func(Record) error) error {
	path := s.sessionPath(id)

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("セッションのログ %s を読めません: %w", path, err)
	}
	defer f.Close()

	return ReadRecords(f, fn)
}

// ReadRecords は JSONL を読んで 1 レコードずつ fn に渡す。
func ReadRecords(r io.Reader, fn func(Record) error) error {
	scanner := bufio.NewScanner(r)

	// 1 行は短いが、将来レコードが太っても切れないように余裕を持たせる。
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}

		var rec Record
		if err := json.Unmarshal([]byte(text), &rec); err != nil {
			return fmt.Errorf("%d 行目を解釈できません: %w", line, err)
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// ReadSessionStart は生信号ログの先頭にある session_start レコードを読む。
//
// 記録された配線と機種を知るために使う。当時の配線が分からないとポート値を
// 解釈できないので、ログを再生するときはこれを見る。
func ReadSessionStart(path string) (Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return Record{}, fmt.Errorf("生信号ログ %s を読めません: %w", path, err)
	}
	defer f.Close()

	var (
		found Record
		ok    bool
	)
	err = ReadRecords(f, func(rec Record) error {
		if rec.Kind != KindSessionStart {
			// 先頭に無ければ、それ以上探さない。1 行目に必ず来る決まりである。
			return errStopReading
		}
		found, ok = rec, true
		return errStopReading
	})
	if err != nil && !errors.Is(err, errStopReading) {
		return Record{}, err
	}
	if !ok {
		return Record{}, fmt.Errorf("生信号ログ %s に session_start がありません", path)
	}
	return found, nil
}

// errStopReading は読み取りを途中で打ち切るための内部エラー。
var errStopReading = errors.New("store: 読み取りを打ち切ります")
