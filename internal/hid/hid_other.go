//go:build !linux

package hid

import "fmt"

// この OS 向けのトランスポートはまだ書いていない。ビルドは通り、ダミー信号源と
// 記録再生信号源は動くので、集計とフロントの開発はこの OS 上でも進められる。
//
// Windows 版は setupapi.dll と hid.dll を golang.org/x/sys/windows 経由で呼ぶ形で
// 書ける（Win32 API は syscall で呼べるため cgo は不要）。macOS 版は IOKit が
// 必要なため cgo か purego が避けられない。詳細は
// docs/adr/0005-signal-source-abstraction.md を参照。

func enumerate() ([]Info, error) {
	return nil, ErrNotSupported
}

func open(path string) (Device, error) {
	return nil, fmt.Errorf("%s を開けません: %w", path, ErrNotSupported)
}
