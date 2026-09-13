//go:build linux

package opcmd

import (
	"os"

	"golang.org/x/sys/unix"
)

// isTerminal は f が端末かどうかを返す。/dev/null も文字デバイスなので、ファイルの
// 種類では判定せず、端末の属性を取れるかで見る。
func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}
