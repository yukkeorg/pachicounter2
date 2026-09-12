// Package hid は OS 別の HID トランスポートを提供する。
//
// これは信号源の抽象の第 3 段であり、「バイト列を HID デバイスへ届ける」ことだけを
// 受け持つ。バイト列の意味（コマンド形式、レポート長、ポート数）は第 2 段の
// internal/hidgpio が持つ。cgo は使わない。詳細は
// docs/adr/0005-signal-source-abstraction.md を参照。
package hid

import (
	"errors"
	"time"
)

// ErrNotSupported はこの OS 向けの実装がまだ無いことを表す。
var ErrNotSupported = errors.New("hid: この OS 向けのトランスポートは未実装です")

// ErrTimeout は読み取りが期限内に終わらなかったことを表す。
var ErrTimeout = errors.New("hid: 読み取りがタイムアウトしました")

// ErrDisconnected はデバイスが外れたことを表す。信号源はこれを受けて再接続を待つ。
var ErrDisconnected = errors.New("hid: デバイスが外れました")

// Info は列挙で見つかった HID デバイス 1 件。
type Info struct {
	// Path は OS 上の識別子。Linux では /dev/hidrawN、Windows ではデバイス
	// インタフェースのパス。
	Path string

	// VendorID と ProductID は USB の識別子。
	VendorID  uint16
	ProductID uint16

	// Name はデバイスが名乗る名前。表示とログに使う。
	Name string
}

// Device は開かれた HID デバイス。
type Device interface {
	// Write は出力レポートを 1 つ送る。report の先頭バイトはレポート番号として
	// 解釈される。番号付きレポートを使わないデバイスでは、そこがそのまま
	// ペイロードの先頭として扱われる。
	Write(report []byte) error

	// Read は入力レポートを 1 つ受け取る。timeout を過ぎたら ErrTimeout を返す。
	Read(buf []byte, timeout time.Duration) (int, error)

	// Close はデバイスを閉じる。
	Close() error
}

// Enumerate は接続されている HID デバイスを列挙する。
func Enumerate() ([]Info, error) {
	return enumerate()
}

// IDPair は USB の識別子の組。
type IDPair struct {
	VendorID  uint16
	ProductID uint16
}

// Open は Path で指定したデバイスを開く。
func Open(path string) (Device, error) {
	return open(path)
}
