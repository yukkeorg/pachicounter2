//go:build windows

package opcmd

import (
	"os"

	"golang.org/x/sys/windows"
)

// isTerminal は f がコンソールかどうかを返す。
func isTerminal(f *os.File) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(f.Fd()), &mode) == nil
}
