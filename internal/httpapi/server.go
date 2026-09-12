// Package httpapi はコアの HTTP 面を受け持つ。
//
// スナップショットは SSE で push し、操作は POST で受ける。WebSocket ではなく
// SSE なのは、データが実質一方向であり、EventSource の自動再接続によって
// 「フロントを気軽に増やす・落とす・作り直す」がフロント側のコードなしで
// 成立するため。詳細は docs/adr/0001-core-as-resident-http-server.md を参照。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/yukkeorg/pachicounter/api"
	"github.com/yukkeorg/pachicounter/internal/counter"
	"github.com/yukkeorg/pachicounter/pkg/machine"
	"github.com/yukkeorg/pachicounter/web"
)

// heartbeatInterval は SSE の接続を維持するためにコメント行を送る間隔。
const heartbeatInterval = 15 * time.Second

// Controller はコアへの操作。
type Controller interface {
	// Snapshot は最新のスナップショットを返す。
	Snapshot() api.Snapshot

	// Subscribe はスナップショットの流れを受け取る。戻り値の関数で購読をやめる。
	Subscribe() (<-chan api.Snapshot, func())

	// NewSession は新しいセッションを始める。
	NewSession(note string) error

	// Correct はカウンタを手動補正する。
	Correct(req api.CorrectRequest) error
}

// Options はサーバの設定。
type Options struct {
	// Addr は待ち受けアドレス。既定は 127.0.0.1 に閉じる。台のデータを
	// LAN に晒すのは明示的な指定があったときだけとする。
	Addr string

	// AllowOrigin は Access-Control-Allow-Origin に入れる値。
	// React などを別ポートの開発サーバで動かすとオリジンが異なり、
	// EventSource がブロックされるため、そのときだけ設定する。
	AllowOrigin string

	// FrontDir は外部のフロントを置いたディレクトリ。空なら埋め込みを使う。
	FrontDir string

	// Logger はログの出力先。
	Logger *slog.Logger
}

// Server はコアの HTTP サーバ。
type Server struct {
	ctrl Controller
	opts Options
	log  *slog.Logger
	mux  *http.ServeMux
}

// New はサーバを組み立てる。
func New(ctrl Controller, opts Options) (*Server, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:18888"
	}

	front, err := frontFS(opts.FrontDir)
	if err != nil {
		return nil, err
	}

	s := &Server{ctrl: ctrl, opts: opts, log: opts.Logger, mux: http.NewServeMux()}

	s.mux.HandleFunc("GET "+api.PathEvents, s.handleEvents)
	s.mux.HandleFunc("GET "+api.PathSnapshot, s.handleSnapshot)
	s.mux.HandleFunc("GET "+api.PathMachines, s.handleMachines)
	s.mux.HandleFunc("POST "+api.PathSessionNew, s.handleSessionNew)
	s.mux.HandleFunc("POST "+api.PathCorrect, s.handleCorrect)
	s.mux.Handle("GET /", http.FileServerFS(front))

	return s, nil
}

// frontFS は使うフロントのファイル群を決める。外部ディレクトリの指定があれば
// そちらを優先する。
func frontFS(dir string) (fs.FS, error) {
	if dir == "" {
		return web.FS(), nil
	}

	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("フロントのディレクトリ %s を読めません: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s はディレクトリではありません", dir)
	}
	return os.DirFS(dir), nil
}

// Addr は待ち受けアドレスを返す。
func (s *Server) Addr() string { return s.opts.Addr }

// Handler は組み立てた http.Handler を返す。テストから使う。
func (s *Server) Handler() http.Handler { return s.mux }

// Run はサーバを動かし、ctx が終わったら止める。
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.opts.Addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	done := make(chan error, 1)
	go func() {
		s.log.Info("HTTP サーバを開始しました", "addr", s.opts.Addr, "front", s.frontLabel())
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		done <- err
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func (s *Server) frontLabel() string {
	if s.opts.FrontDir == "" {
		return "埋め込み"
	}
	return s.opts.FrontDir
}

// handleEvents はスナップショットを SSE で流し続ける。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "このサーバは SSE を流せません", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// 間にプロキシが入ってもバッファされないようにする。
	h.Set("X-Accel-Buffering", "no")
	s.setCORS(h)

	w.WriteHeader(http.StatusOK)

	updates, cancel := s.ctrl.Subscribe()
	defer cancel()

	// 繋いだ直後に今の状態を送る。途中から接続したフロントが空のまま
	// 待たされないようにするためで、スナップショットを毎回全体で送ると
	// 決めているからこれで足りる。
	if err := writeEvent(w, s.ctrl.Snapshot()); err != nil {
		return
	}
	flusher.Flush()

	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return

		case snap, ok := <-updates:
			if !ok {
				return
			}
			if err := writeEvent(w, snap); err != nil {
				return
			}
			flusher.Flush()

		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, snap api.Snapshot) error {
	payload, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", api.EventName, payload)
	return err
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, s.ctrl.Snapshot())
}

func (s *Server) handleMachines(w http.ResponseWriter, r *http.Request) {
	all := machine.All()

	items := make([]api.MachineListItem, 0, len(all))
	for _, reg := range all {
		items = append(items, api.MachineListItem{
			ID:          reg.ID,
			DisplayName: reg.DisplayName,
			Variants:    reg.Variants,
		})
	}
	s.writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleSessionNew(w http.ResponseWriter, r *http.Request) {
	var req api.SessionNewRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			s.writeError(w, http.StatusBadRequest, fmt.Errorf("本文を解釈できません: %w", err))
			return
		}
	}

	if err := s.ctrl.NewSession(req.Note); err != nil {
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, s.ctrl.Snapshot())
}

func (s *Server) handleCorrect(w http.ResponseWriter, r *http.Request) {
	var req api.CorrectRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, fmt.Errorf("本文を解釈できません: %w", err))
		return
	}

	if req.Counter == "" {
		s.writeError(w, http.StatusBadRequest,
			fmt.Errorf("補正するカウンタが指定されていません（補正できるカウンタ: %v）", counter.CorrectableNames()))
		return
	}
	if req.Delta == 0 {
		s.writeError(w, http.StatusBadRequest, errors.New("補正量が 0 です"))
		return
	}

	if err := s.ctrl.Correct(req); err != nil {
		s.writeError(w, http.StatusBadRequest, err)
		return
	}
	s.writeJSON(w, http.StatusOK, s.ctrl.Snapshot())
}

func (s *Server) setCORS(h http.Header) {
	if s.opts.AllowOrigin == "" {
		return
	}
	h.Set("Access-Control-Allow-Origin", s.opts.AllowOrigin)
	h.Set("Vary", "Origin")
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, body any) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	s.setCORS(h)
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.log.Warn("応答を書けませんでした", "err", err)
	}
}

func (s *Server) writeError(w http.ResponseWriter, status int, err error) {
	s.writeJSON(w, status, api.ErrorResponse{Error: err.Error()})
}
