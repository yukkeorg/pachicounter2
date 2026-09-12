package app

import (
	"sync"

	"github.com/yukkeorg/pachicounter2/api"
)

// hub はスナップショットを購読者へ配る。
//
// 購読者ごとに容量 1 の枠を持ち、配る前に古い分を捨てる。スナップショットは
// 差分ではなく毎回全体なので、新しいものが届けば古いものは要らない。遅い購読者が
// 集計を止めないようにするためで、詰まっても最新の 1 つは必ず残る。
type hub struct {
	mu   sync.Mutex
	subs map[chan api.Snapshot]struct{}
}

func newHub() *hub {
	return &hub{subs: map[chan api.Snapshot]struct{}{}}
}

func (h *hub) subscribe() (<-chan api.Snapshot, func()) {
	ch := make(chan api.Snapshot, 1)

	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()

	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()

		if _, ok := h.subs[ch]; !ok {
			return
		}
		delete(h.subs, ch)
		close(ch)
	}
	return ch, cancel
}

func (h *hub) broadcast(snap api.Snapshot) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for ch := range h.subs {
		select {
		case <-ch:
		default:
		}

		select {
		case ch <- snap:
		default:
		}
	}
}
