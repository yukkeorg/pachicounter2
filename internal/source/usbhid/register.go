package usbhid

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/yukkeorg/pachicounter2/internal/hidgpio"
	"github.com/yukkeorg/pachicounter2/internal/source"
	"github.com/yukkeorg/pachicounter2/pkg/signal"
)

func init() {
	source.Register(source.Registration{
		Name:    "usbhid",
		Summary: "USB-HID の GPIO デバイスで台の信号を読む",
		Usage:   "usbhid[:driver=<ドライバ名>,interval=<ポーリング間隔>]",
		Kind:    source.KindLive,
		New: func(arg string, env source.Env) (signal.Source, error) {
			opts, err := parseArg(arg)
			if err != nil {
				return nil, err
			}
			opts.Logger = env.Logger
			return New(opts), nil
		},
		PrintDetails: printDetails,
	})
}

// parseArg は -source usbhid: の引数を読む。driver=<名前>,interval=<間隔> の形で、
// どちらも省略できる。知らない設定はエラーにする。黙って無視すると、書き間違いに
// 気づけないまま既定値で動いてしまう。
func parseArg(arg string) (Options, error) {
	var opts Options
	seen := map[string]bool{}

	for _, part := range strings.Split(arg, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return Options{}, fmt.Errorf("usbhid の設定 %q は key=value の形ではありません（例: usbhid:driver=usbio2）", part)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		if seen[key] {
			return Options{}, fmt.Errorf("usbhid の設定 %q が二重に指定されています", key)
		}
		seen[key] = true

		switch key {
		case "driver":
			if value == "" {
				return Options{}, errors.New("usbhid の driver が空です")
			}
			opts.Driver = value

		case "interval":
			d, err := time.ParseDuration(value)
			if err != nil {
				return Options{}, fmt.Errorf("usbhid の interval %q を解釈できません（例: 5ms）: %w", value, err)
			}
			if d <= 0 {
				return Options{}, fmt.Errorf("usbhid の interval は 0 より大きくしてください: %s", value)
			}
			opts.Interval = d

		default:
			return Options{}, fmt.Errorf("usbhid に %q という設定はありません（使える設定: driver, interval）", key)
		}
	}
	return opts, nil
}

// printDetails は登録されているドライバと、今つながっているデバイスの検出結果を書く。
// デバイスは開かず、列挙するだけである。
func printDetails(w io.Writer) {
	names := hidgpio.Names()
	if len(names) == 0 {
		fmt.Fprintln(w, "ドライバ: 登録されていません")
		return
	}
	for _, name := range names {
		driver, _ := hidgpio.Lookup(name)
		fmt.Fprintf(w, "ドライバ: %-8s %s（入力 %d ビット）\n", driver.Name, driver.DisplayName, driver.NumBits)
	}

	found, err := hidgpio.Detect("")
	switch {
	case errors.Is(err, hidgpio.ErrNoDevice):
		fmt.Fprintln(w, "検出結果: 対応するデバイスは見つかりませんでした")
	case err != nil:
		fmt.Fprintf(w, "検出結果: 検出に失敗しました: %v\n", err)
	default:
		fmt.Fprintf(w, "検出結果: %s を %s で検出しました（VID 0x%04x / PID 0x%04x）\n",
			found.Driver.DisplayName, found.Info.Path, found.Info.VendorID, found.Info.ProductID)
	}
}
