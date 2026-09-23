package hidpin

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/yukkeorg/hidpin/host-go/hidpin"
	"github.com/yukkeorg/pachicounter2/internal/source"
	"github.com/yukkeorg/pachicounter2/pkg/signal"
)

func init() {
	source.Register(source.Registration{
		Name:    "hidpin",
		Summary: "hidpin（RP2040 で作った USB-HID の GPIO デバイス）で台の信号を読む",
		Usage:   "hidpin[:serial=<シリアル番号>]",
		Kind:    source.KindLive,
		New: func(arg string, env source.Env) (signal.Source, error) {
			opts, err := parseArg(arg)
			if err != nil {
				return nil, err
			}
			// 使えない OS では、デバイスを待ち続けるのではなく起動の時点で止める。
			if _, err := hidpin.FindDevices(); errors.Is(err, hidpin.ErrUnsupportedPlatform) {
				return nil, errors.New("hidpin は Linux でだけ使えます")
			}
			opts.Logger = env.Logger
			opts.Wiring = env.Wiring
			return New(opts), nil
		},
		PrintDetails: printDetails,
	})
}

// parseArg は -source hidpin: の引数を読む。serial=<シリアル番号> の形で、省略できる。
// 知らない設定はエラーにする。黙って無視すると、書き間違いに気づけないまま別の
// デバイスを読んでしまう。
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
			return Options{}, fmt.Errorf("hidpin の設定 %q は key=value の形ではありません（例: hidpin:serial=E660583883693A2F）", part)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		if seen[key] {
			return Options{}, fmt.Errorf("hidpin の設定 %q が二重に指定されています", key)
		}
		seen[key] = true

		switch key {
		case "serial":
			if value == "" {
				return Options{}, errors.New("hidpin の serial が空です")
			}
			opts.Serial = value

		default:
			return Options{}, fmt.Errorf("hidpin に %q という設定はありません（使える設定: serial）", key)
		}
	}
	return opts, nil
}

// printDetails は今つながっている hidpin を書く。デバイスは開かず、列挙するだけである。
func printDetails(w io.Writer) {
	entries, err := hidpin.FindDevices()
	switch {
	case errors.Is(err, hidpin.ErrUnsupportedPlatform):
		fmt.Fprintln(w, "検出結果: hidpin は Linux でだけ使えます")
	case err != nil:
		fmt.Fprintf(w, "検出結果: 検出に失敗しました: %v\n", err)
	case len(entries) == 0:
		fmt.Fprintln(w, "検出結果: hidpin は見つかりませんでした")
	default:
		for _, e := range entries {
			fmt.Fprintf(w, "検出結果: %s（シリアル番号 %s）を %s で検出しました\n", e.Product, e.Serial, e.Path)
		}
	}
}
