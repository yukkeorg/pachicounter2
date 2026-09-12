// Package hidgpio は USB-HID で GPIO を出す製品のプロトコルを抽象する。
//
// これは信号源の抽象の第 2 段である。USB-HID で GPIO を出す製品は複数あり
// （km2net USB-IO 2.0、Microchip MCP2221A、FTDI FT260 など）、VID/PID もレポート長も
// コマンド形式も違うため、製品ごとのドライバを差し替えられるようにしてある。
// バイト列を届ける部分は第 3 段の internal/hid が受け持つ。詳細は
// docs/adr/0005-signal-source-abstraction.md を参照。
package hidgpio

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/yukkeorg/pachicounter2/internal/hid"
	"github.com/yukkeorg/pachicounter2/pkg/signal"
)

// ErrNoDevice は対応するデバイスが 1 つも見つからなかったことを表す。
// 信号源はこれを受けて接続を待ち続ける。
var ErrNoDevice = errors.New("hidgpio: 対応するデバイスが見つかりません")

// Config はデバイスを開くときの設定。
//
// 入力ピンのプルアップや入出力方向はデバイス側のフラッシュに永続保存される類の設定で
// あることが多く（USB-IO 2.0 はそうである）、常用経路で書き込むとフラッシュを
// 摩耗させ、デバイスの状態を黙って書き換えることになる。よってここには含めない。
// 現行の Python 実装も設定の書き込みは一度も行っていない。
type Config struct {
	// ReadTimeout は 1 回の読み取りに許す時間。
	ReadTimeout time.Duration
}

// Conn は開かれたデバイスとの接続。
type Conn interface {
	// ReadPorts は入力ビットを読む。返す値は生のポート値であり、ビット位置と
	// 信号の役割の対応は台ごとの配線で決まるため、ここでは解釈しない。
	ReadPorts() (signal.Ports, error)

	// Close は接続を閉じる。
	Close() error
}

// Driver は HID GPIO 製品 1 種類のプロトコル。
type Driver struct {
	// Name はドライバの名前。設定での明示指定に使う。
	Name string

	// DisplayName は人間に見せる製品名。
	DisplayName string

	// IDs は自動検出に使う USB 識別子の組。
	IDs []hid.IDPair

	// NumBits は読み取れる入力ビット数。
	NumBits int

	// Open は開かれた HID デバイスの上に接続を作る。
	Open func(d hid.Device, cfg Config) (Conn, error)
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Driver{}
)

// Register はドライバを登録する。実装パッケージの init() から呼ぶ。
func Register(d Driver) {
	if d.Name == "" {
		panic("hidgpio: Register に Name の無い Driver が渡された")
	}
	if d.Open == nil {
		panic("hidgpio: Register に Open の無い Driver が渡された: " + d.Name)
	}

	registryMu.Lock()
	defer registryMu.Unlock()

	if _, dup := registry[d.Name]; dup {
		panic("hidgpio: ドライバ名が二重に登録された: " + d.Name)
	}
	registry[d.Name] = d
}

// Lookup は名前でドライバを引く。
func Lookup(name string) (Driver, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()

	d, ok := registry[name]
	return d, ok
}

// Names は登録済みドライバの名前を昇順で返す。
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

// Found は検出されたデバイス 1 件。
type Found struct {
	Driver Driver
	Info   hid.Info
}

// Detect は接続されているデバイスのうち、登録済みドライバが対応するものを探す。
// driverName が空でなければそのドライバに限定する。
func Detect(driverName string) (Found, error) {
	registryMu.RLock()
	drivers := make([]Driver, 0, len(registry))
	if driverName != "" {
		d, ok := registry[driverName]
		if !ok {
			registryMu.RUnlock()
			return Found{}, fmt.Errorf("ドライバ %q は登録されていません（利用できるドライバ: %v）", driverName, Names())
		}
		drivers = append(drivers, d)
	} else {
		for _, d := range registry {
			drivers = append(drivers, d)
		}
		sort.Slice(drivers, func(i, j int) bool { return drivers[i].Name < drivers[j].Name })
	}
	registryMu.RUnlock()

	all, err := hid.Enumerate()
	if err != nil {
		return Found{}, err
	}

	for _, d := range drivers {
		for _, want := range d.IDs {
			for _, info := range all {
				if info.VendorID == want.VendorID && info.ProductID == want.ProductID {
					return Found{Driver: d, Info: info}, nil
				}
			}
		}
	}
	return Found{}, ErrNoDevice
}

// OpenDetected は検出したデバイスを開いて接続を作る。
func (f Found) Open(cfg Config) (Conn, error) {
	dev, err := hid.Open(f.Info.Path)
	if err != nil {
		return nil, err
	}

	conn, err := f.Driver.Open(dev, cfg)
	if err != nil {
		_ = dev.Close()
		return nil, err
	}
	return conn, nil
}
