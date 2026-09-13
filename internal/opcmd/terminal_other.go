//go:build !linux && !windows

package opcmd

import "os"

// isTerminal は、端末の属性を調べる手段を用意していない OS では、文字デバイスかどうかで
// 代用する。/dev/null も端末とみなしてしまうが、この OS は今のところ対象外である。
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
