package opcmd

import (
	"context"
	"os/exec"
	"time"
)

// notifyDesktop はデスクトップ通知を出す。notify-send が無い環境や、出せなかったときは
// 何もしない。失敗は標準エラーにも出しているので、通知は補助である。
func notifyDesktop(summary, body string) {
	path, err := exec.LookPath("notify-send")
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, path, "--app-name=PachiCounter", summary, body).Run()
}
