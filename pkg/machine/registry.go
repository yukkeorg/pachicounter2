package machine

import (
	"fmt"
	"sort"
	"sync"
)

// Factory は機種プラグインを 1 つ作る。variant はスペック違いの識別子で、
// 区別が無い機種では空文字列が渡る。
type Factory func(variant string) (Plugin, error)

// Registration はレジストリに登録された 1 機種の情報。
type Registration struct {
	// ID は機種の識別子。
	ID string

	// DisplayName は人間に見せる機種名。バリアントを持つ機種では代表的な名前。
	DisplayName string

	// Variants は選べるバリアントの一覧。区別が無い機種では空。
	Variants []string

	// New はプラグインを作る。
	New Factory
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Registration{}
)

// Register は機種プラグインを登録する。実装パッケージの init() から呼ぶ。
// 同じ ID を二重に登録した場合は panic する（コンパイル時登録なので、これは
// 実行時の入力ではなくプログラムの誤りである）。
func Register(r Registration) {
	if r.ID == "" {
		panic("machine: Register に ID の無い Registration が渡された")
	}
	if r.New == nil {
		panic("machine: Register に New の無い Registration が渡された: " + r.ID)
	}

	registryMu.Lock()
	defer registryMu.Unlock()

	if _, dup := registry[r.ID]; dup {
		panic("machine: 機種 ID が二重に登録された: " + r.ID)
	}
	registry[r.ID] = r
}

// New は登録済みの機種プラグインを作る。
func New(id, variant string) (Plugin, error) {
	registryMu.RLock()
	r, ok := registry[id]
	registryMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("機種 %q は登録されていません（利用できる機種: %v）", id, IDs())
	}

	if len(r.Variants) > 0 {
		if variant == "" {
			return nil, fmt.Errorf("機種 %q はバリアントの指定が必要です（選べるバリアント: %v）", id, r.Variants)
		}
		if !contains(r.Variants, variant) {
			return nil, fmt.Errorf("機種 %q にバリアント %q はありません（選べるバリアント: %v）", id, variant, r.Variants)
		}
	} else if variant != "" {
		return nil, fmt.Errorf("機種 %q にバリアントの区別はありません", id)
	}

	return r.New(variant)
}

// All は登録済みの機種を ID 順に返す。
func All() []Registration {
	registryMu.RLock()
	defer registryMu.RUnlock()

	out := make([]Registration, 0, len(registry))
	for _, r := range registry {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// IDs は登録済みの機種 ID を昇順で返す。
func IDs() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()

	out := make([]string, 0, len(registry))
	for id := range registry {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
