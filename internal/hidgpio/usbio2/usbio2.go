// Package usbio2 は km2net USB-IO 2.0 のプロトコルを実装する。
//
// プロトコルは 64 バイト固定のレポートで、先頭バイトがコマンド、末尾バイトが
// シーケンス番号、その間がペイロードである。読み取りは「コマンドを書いてから
// 応答を読む」形なので、デバイスから勝手にデータが来ることはない。
//
// 仕様: http://km2net.com/usb-io2.0/index.shtml
package usbio2

import (
	"fmt"
	"sync"
	"time"

	"github.com/yukkeorg/pachicounter2/internal/hid"
	"github.com/yukkeorg/pachicounter2/internal/hidgpio"
	"github.com/yukkeorg/pachicounter2/pkg/signal"
)

const (
	// DriverName は設定で指定するときの名前。
	DriverName = "usbio2"

	// vendorID は km2net。
	vendorID = 0x1352

	// productIDOriginal は USB-IO2.0、productIDAkizuki は秋月電子の互換品。
	productIDOriginal = 0x0120
	productIDAkizuki  = 0x0121

	// reportSize はレポート長。コマンドも応答もこの長さで固定。
	reportSize = 64

	// seqIndex はシーケンス番号が入る位置。
	seqIndex = reportSize - 1

	// cmdReadSend は入力を読んで出力を書き込むコマンド。応答の 1 バイト目から
	// Port0、2 バイト目から Port1 の値が返る。
	cmdReadSend = 0x20

	// numBits は読み取れる入力ビット数。Port0 が 8 ビット、Port1 が 4 ビット。
	numBits = 12
)

func init() {
	hidgpio.Register(hidgpio.Driver{
		Name:        DriverName,
		DisplayName: "km2net USB-IO 2.0",
		IDs: []hid.IDPair{
			{VendorID: vendorID, ProductID: productIDOriginal},
			{VendorID: vendorID, ProductID: productIDAkizuki},
		},
		NumBits: numBits,
		Open:    open,
	})
}

type conn struct {
	mu      sync.Mutex
	dev     hid.Device
	timeout time.Duration
	seq     uint8

	// cmd と resp は毎回の読み取りで使い回す。5ms 間隔で回すので、
	// 1 回ごとに確保すると無駄にゴミを作る。
	cmd  [reportSize]byte
	resp [reportSize + 1]byte
}

func open(d hid.Device, cfg hidgpio.Config) (hidgpio.Conn, error) {
	timeout := cfg.ReadTimeout
	if timeout <= 0 {
		timeout = 50 * time.Millisecond
	}

	c := &conn{dev: d, timeout: timeout}

	// 開いた直後に 1 回読んで、実際に応答が返るデバイスかを確かめる。
	// VID/PID が合っているだけでは、権限や配線の問題は分からない。
	if _, err := c.ReadPorts(); err != nil {
		return nil, fmt.Errorf("USB-IO 2.0 の応答を確認できません: %w", err)
	}
	return c, nil
}

// ReadPorts は Port0 と Port1 を読んで 1 つの値にまとめて返す。
// 下位 8 ビットが Port0、続く 4 ビットが Port1 である。
//
// 返す値はデバイスが返した生の値であり、反転（アクティブロー）は行わない。
// 台の外部出力端子はオープンコレクタで、プルアップ抵抗と組み合わせると信号が
// 出ている間 LOW になるが、それは配線の性質であってデバイスの性質ではないため、
// 反転はビットと役割を対応づける層で行う。生の値のまま記録することで、
// 生信号ログが実際に観測した事実そのものになる。
func (c *conn) ReadPorts() (signal.Ports, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.seq++
	seq := c.seq

	for i := range c.cmd {
		c.cmd[i] = 0
	}
	c.cmd[0] = cmdReadSend
	c.cmd[seqIndex] = seq

	if err := c.dev.Write(c.cmd[:]); err != nil {
		return 0, err
	}

	deadline := time.Now().Add(c.timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, hid.ErrTimeout
		}

		n, err := c.dev.Read(c.resp[:], remaining)
		if err != nil {
			return 0, err
		}

		payload, ok := frame(c.resp[:n], cmdReadSend, seq)
		if !ok {
			// 別のコマンドの応答や、前回の取りこぼしが残っていることがある。
			// 期限まで読み直す。
			continue
		}

		// payload[0] がコマンドのエコー、payload[1] が Port0、payload[2] が Port1。
		port0 := uint16(payload[1])
		port1 := uint16(payload[2]) & 0x0f
		return signal.Ports(port1<<8 | port0), nil
	}
}

// frame は応答からレポート本体を切り出す。
//
// hidraw は番号付きレポートを使うデバイスでは先頭にレポート番号を付けて返すため、
// 本体が 0 バイト目から始まるとは限らない。コマンドのエコーとシーケンス番号の
// 位置で本体を特定する。
func frame(buf []byte, cmd, seq uint8) ([]byte, bool) {
	for _, offset := range []int{0, 1} {
		if len(buf) < offset+reportSize {
			continue
		}
		body := buf[offset : offset+reportSize]
		if body[0] == cmd && body[seqIndex] == seq {
			return body, true
		}
	}
	return nil, false
}

func (c *conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.dev == nil {
		return nil
	}
	dev := c.dev
	c.dev = nil
	return dev.Close()
}
