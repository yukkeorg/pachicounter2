package opcmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/yukkeorg/pachicounter2/api"
)

// coreConn はコアの HTTP API への窓口。
type coreConn struct {
	addr   string
	client *http.Client
}

// newCoreConn は addr のコアへの窓口を作る。addr はコアの -listen と同じ host:port の形。
func newCoreConn(addr string, client *http.Client) (*coreConn, error) {
	if strings.Contains(addr, "://") {
		return nil, usagef("-core には host:port の形でアドレスを指定してください（例: %s）: %s", api.DefaultAddr, addr)
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return nil, usagef("-core のアドレスを読めません（例: %s）: %s", api.DefaultAddr, addr)
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &coreConn{addr: addr, client: client}, nil
}

func (c *coreConn) snapshot() (api.Snapshot, error) {
	return c.do(http.MethodGet, api.PathSnapshot, nil)
}

func (c *coreConn) correct(req api.CorrectRequest) (api.Snapshot, error) {
	return c.do(http.MethodPost, api.PathCorrect, req)
}

func (c *coreConn) newSession(note string) (api.Snapshot, error) {
	return c.do(http.MethodPost, api.PathSessionNew, api.SessionNewRequest{Note: note})
}

// do はコアに頼み、応答のスナップショットを返す。コアが断ったときは、コアが返した理由を
// そのままエラーにする。
func (c *coreConn) do(method, path string, body any) (api.Snapshot, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return api.Snapshot{}, err
		}
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequest(method, "http://"+c.addr+path, reader)
	if err != nil {
		return api.Snapshot{}, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return api.Snapshot{}, fmt.Errorf("コアに接続できません（%s）。コアが起動しているか確かめてください", c.addr)
		}
		return api.Snapshot{}, fmt.Errorf("コア（%s）と通信できません: %w", c.addr, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return api.Snapshot{}, fmt.Errorf("コアの応答を読めません: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var e api.ErrorResponse
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return api.Snapshot{}, errors.New(e.Error)
		}
		return api.Snapshot{}, fmt.Errorf("コアが HTTP %d を返しました", resp.StatusCode)
	}

	var snap api.Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return api.Snapshot{}, fmt.Errorf("コアの応答を解釈できません: %w", err)
	}
	return snap, nil
}
