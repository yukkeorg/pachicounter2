package httpapi

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yukkeorg/pachicounter2/api"
)

// fakeController はスナップショットを 1 つ返すだけのコア。購読の流れには何も流さない。
type fakeController struct{}

func (fakeController) Snapshot() api.Snapshot { return api.Snapshot{Schema: api.SchemaVersion} }

func (fakeController) Subscribe() (<-chan api.Snapshot, func()) {
	return make(chan api.Snapshot), func() {}
}

func (fakeController) NewSession(string) error { return nil }

func (fakeController) Correct(api.CorrectRequest) error { return nil }

func TestServeStopsPromptlyWithOpenEventStream(t *testing.T) {
	s, err := New(fakeController{}, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("サーバを組み立てられません: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("待ち受けられません: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- s.serve(ctx, ln) }()

	// ブラウザや OBS が開いている状態を作る。/events に繋ぎ、最初のスナップショットが
	// 届くまで待つ。
	resp, err := http.Get("http://" + ln.Addr().String() + api.PathEvents)
	if err != nil {
		t.Fatalf("/events に繋げません: %v", err)
	}
	defer resp.Body.Close()

	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "event: ") {
		t.Fatalf("最初のイベントを受け取れません: %q, %v", line, err)
	}

	// Ctrl+C に当たる。流し続ける接続があっても、待たされずにエラー無しで止まること。
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("止めたときにエラーが返った: %v", err)
		}
	case <-time.After(shutdownTimeout / 2):
		t.Fatalf("/events が繋がっていると %s 以内に止まらない", shutdownTimeout/2)
	}
}
