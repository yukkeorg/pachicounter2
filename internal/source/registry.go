// Package source は信号源の名簿を持つ。
//
// 機種プラグインと同じく動的ロードはせず、実装パッケージが init() で自己登録する。
// -source には「名前」か「名前:引数」を渡し、引数の解釈は信号源ごとに任せる。
// usbhid と hidpin は key=value のカンマ区切り、file と loop は生信号ログのパスそのものを取る。
// 詳細は docs/adr/0005-signal-source-abstraction.md を参照。
package source

import (
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/yukkeorg/pachicounter2/internal/config"
	"github.com/yukkeorg/pachicounter2/pkg/signal"
)

// Kind は信号源の性質。保存先に書くかどうかはこれで決まる。
type Kind int

const (
	// KindLive は台の信号を今読んでいる信号源。セッションを続け、生信号ログと
	// スナップショットを保存する。
	KindLive Kind = iota + 1

	// KindReplay は記録を流し直す信号源。流している記録がすでに記録そのものなので、
	// 保存先には何も書かない。
	KindReplay
)

// Env は信号源を作るときに渡す、どの信号源にも共通の道具。信号源ごとの設定は
// ここに置かず、-source の引数で渡す。
type Env struct {
	// Logger はログの出力先。
	Logger *slog.Logger

	// Wiring は指定された配線。信号源が、その配線で信号を読めるかを確かめるために使う
	// （hidpin は、監視していない GPIO を指していないかを見る）。記録を流す信号源では、
	// この後で記録された配線に入れ替わることがあるので当てにしない。
	Wiring config.Wiring
}

// Registration は名簿に載せる信号源 1 種類。
type Registration struct {
	// Name は -source で指定する名前。
	Name string

	// Summary は一覧に出す短い説明。
	Summary string

	// Usage は -source への書き方。
	Usage string

	// Kind は信号源の性質。
	Kind Kind

	// New は信号源を作る。arg は -source の最初の ":" より後ろで、無ければ空文字列。
	New func(arg string, env Env) (signal.Source, error)

	// PrintDetails は一覧に添える詳しい情報を書く。無ければ nil。usbhid はドライバの
	// 一覧と検出結果をここで書く。
	PrintDetails func(w io.Writer)
}

// Recorded は記録したときの機種と配線。
type Recorded struct {
	Machine string
	Variant string
	Wiring  config.Wiring
}

// Recording は記録を流す信号源が実装する。
//
// 「保存先に書かない」（KindReplay）と「記録したときの設定を持つ」は似ているが別の
// 性質なので、別々に宣言する。
type Recording interface {
	// Recorded は記録したときの機種と配線を返す。
	Recorded() (Recorded, error)
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Registration{}
)

// Register は信号源を名簿に載せる。実装パッケージの init() から呼ぶ。
// 誤りは実行時の入力ではなくプログラムの誤りなので panic する。
func Register(r Registration) {
	if r.Name == "" {
		panic("source: Register に Name の無い Registration が渡された")
	}
	if strings.Contains(r.Name, ":") {
		panic("source: 信号源の名前に : は使えない: " + r.Name)
	}
	if r.Kind != KindLive && r.Kind != KindReplay {
		panic("source: Register に Kind の無い Registration が渡された: " + r.Name)
	}
	if r.New == nil {
		panic("source: Register に New の無い Registration が渡された: " + r.Name)
	}

	registryMu.Lock()
	defer registryMu.Unlock()

	if _, dup := registry[r.Name]; dup {
		panic("source: 信号源の名前が二重に登録された: " + r.Name)
	}
	registry[r.Name] = r
}

// Open は -source の指定から信号源を作る。
//
// 指定は最初の ":" で名前と引数に分ける。Windows のパス（C:\...）を引数に
// そのまま書けるようにするため、2 つ目以降の ":" は引数に含める。
func Open(spec string, env Env) (signal.Source, Registration, error) {
	name, arg, _ := strings.Cut(spec, ":")

	registryMu.RLock()
	r, ok := registry[name]
	registryMu.RUnlock()

	if !ok {
		return nil, Registration{}, fmt.Errorf("信号源 %q はありません（使える信号源: %s。-list-sources で書き方が出ます）",
			name, strings.Join(Names(), ", "))
	}

	src, err := r.New(arg, env)
	if err != nil {
		return nil, Registration{}, err
	}
	return src, r, nil
}

// All は名簿の信号源を名前順に返す。
func All() []Registration {
	registryMu.RLock()
	defer registryMu.RUnlock()

	out := make([]Registration, 0, len(registry))
	for _, r := range registry {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Names は名簿の信号源の名前を昇順で返す。
func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()

	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
