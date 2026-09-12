package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yukkeorg/pachicounter/api"
)

// Pointer は「今どのセッションを集計しているか」を指す。スナップショットと
// 一緒に保存し、起動時にセッションを続けるために読む。
type Pointer struct {
	Session string `json:"session"`
	Machine string `json:"machine"`
	Variant string `json:"variant,omitempty"`
}

// StoredSnapshot はスナップショットファイルの中身。
type StoredSnapshot struct {
	Pointer  Pointer      `json:"pointer"`
	Snapshot api.Snapshot `json:"snapshot"`
}

// SaveSnapshot はスナップショットを保存する。
//
// 一時ファイルへ書いてから rename する。書いている途中で電源が落ちても、
// 読み手は古い方か新しい方のどちらかを必ず読めて、壊れた中間状態を見ない。
func (s *Store) SaveSnapshot(p Pointer, snap api.Snapshot) error {
	data, err := json.MarshalIndent(StoredSnapshot{Pointer: p, Snapshot: snap}, "", "  ")
	if err != nil {
		return fmt.Errorf("スナップショットを JSON にできません: %w", err)
	}
	data = append(data, '\n')

	final := s.snapshotPath()
	tmp, err := os.CreateTemp(filepath.Dir(final), ".snapshot-*.json")
	if err != nil {
		return fmt.Errorf("一時ファイルを作れません: %w", err)
	}
	tmpName := tmp.Name()

	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("一時ファイルに書けません: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("一時ファイルを同期できません: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("一時ファイルを閉じられません: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("スナップショットを差し替えられません: %w", err)
	}
	return nil
}

// LoadPointer は最後に集計していたセッションを返す。
func (s *Store) LoadPointer() (Pointer, error) {
	data, err := os.ReadFile(s.snapshotPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Pointer{}, ErrNoSession
		}
		return Pointer{}, err
	}

	var stored StoredSnapshot
	if err := json.Unmarshal(data, &stored); err != nil {
		// スナップショットは導出物なので、壊れていても致命的ではない。
		// 続けられるセッションが無いものとして扱う。
		return Pointer{}, ErrNoSession
	}
	if stored.Pointer.Session == "" {
		return Pointer{}, ErrNoSession
	}
	return stored.Pointer, nil
}
