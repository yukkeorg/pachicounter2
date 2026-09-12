//go:build linux

package hid

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// sysfsHidraw は hidraw デバイスの情報が置かれる sysfs のディレクトリ。
const sysfsHidraw = "/sys/class/hidraw"

// enumerate は sysfs を読んで hidraw デバイスを列挙する。libusb も hidapi も使わない。
func enumerate() ([]Info, error) {
	entries, err := os.ReadDir(sysfsHidraw)
	if err != nil {
		if os.IsNotExist(err) {
			// hidraw を持つデバイスが 1 つも無い環境。エラーではない。
			return nil, nil
		}
		return nil, fmt.Errorf("%s を読めません: %w", sysfsHidraw, err)
	}

	var out []Info
	for _, e := range entries {
		name := e.Name() // hidraw0 など
		uevent := filepath.Join(sysfsHidraw, name, "device", "uevent")
		raw, err := os.ReadFile(uevent)
		if err != nil {
			// 読めないデバイスは黙って飛ばす。列挙の途中で止めるほどの事ではない。
			continue
		}

		info := Info{Path: filepath.Join("/dev", name)}
		if err := parseUevent(string(raw), &info); err != nil {
			continue
		}
		out = append(out, info)
	}
	return out, nil
}

// parseUevent は hidraw の uevent を解釈する。
//
// HID_ID は "バス:ベンダID:プロダクトID" の形で、16 進数がゼロ埋めで並ぶ。
// 例: HID_ID=0003:00001352:00000120
func parseUevent(text string, info *Info) error {
	var gotID bool
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}

		switch key {
		case "HID_ID":
			fields := strings.Split(value, ":")
			if len(fields) != 3 {
				continue
			}
			vid, err := strconv.ParseUint(fields[1], 16, 32)
			if err != nil {
				continue
			}
			pid, err := strconv.ParseUint(fields[2], 16, 32)
			if err != nil {
				continue
			}
			info.VendorID = uint16(vid)
			info.ProductID = uint16(pid)
			gotID = true
		case "HID_NAME":
			info.Name = value
		}
	}

	if !gotID {
		return errors.New("uevent に HID_ID がありません")
	}
	return nil
}

// linuxDevice は /dev/hidrawN を直接読み書きする。
//
// O_NONBLOCK で開いて poll(2) で待つ。ブロッキング読み取りにすると、デバイスが
// 応答しないまま goroutine が永久に止まり、別 goroutine から閉じて起こす形になって
// 競合しやすい。
type linuxDevice struct {
	mu   sync.Mutex
	fd   int
	path string
}

func open(path string) (Device, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.EACCES) {
			return nil, fmt.Errorf("%s を開けません（権限がありません。udev ルールの設定が必要です）: %w", path, err)
		}
		if errors.Is(err, unix.ENOENT) {
			return nil, fmt.Errorf("%s がありません: %w", path, ErrDisconnected)
		}
		return nil, fmt.Errorf("%s を開けません: %w", path, err)
	}

	return &linuxDevice{fd: fd, path: path}, nil
}

func (d *linuxDevice) Write(report []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.fd < 0 {
		return os.ErrClosed
	}

	for {
		n, err := unix.Write(d.fd, report)
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.ENODEV), errors.Is(err, unix.ENXIO), errors.Is(err, unix.ESHUTDOWN):
			return fmt.Errorf("%s への書き込みに失敗: %w", d.path, ErrDisconnected)
		case err != nil:
			return fmt.Errorf("%s への書き込みに失敗: %w", d.path, err)
		case n != len(report):
			return fmt.Errorf("%s へ %d バイト書くはずが %d バイトしか書けませんでした", d.path, len(report), n)
		}
		return nil
	}
}

func (d *linuxDevice) Read(buf []byte, timeout time.Duration) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.fd < 0 {
		return 0, os.ErrClosed
	}

	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, ErrTimeout
		}

		fds := []unix.PollFd{{Fd: int32(d.fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int(remaining.Milliseconds())+1)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("%s の待ち受けに失敗: %w", d.path, err)
		}
		if n == 0 {
			return 0, ErrTimeout
		}
		if fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return 0, fmt.Errorf("%s が応答しません: %w", d.path, ErrDisconnected)
		}

		got, err := unix.Read(d.fd, buf)
		switch {
		case errors.Is(err, unix.EINTR), errors.Is(err, unix.EAGAIN):
			continue
		case errors.Is(err, unix.ENODEV), errors.Is(err, unix.ENXIO), errors.Is(err, unix.ESHUTDOWN):
			return 0, fmt.Errorf("%s の読み取りに失敗: %w", d.path, ErrDisconnected)
		case err != nil:
			return 0, fmt.Errorf("%s の読み取りに失敗: %w", d.path, err)
		}
		return got, nil
	}
}

func (d *linuxDevice) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.fd < 0 {
		return nil
	}
	fd := d.fd
	d.fd = -1
	return unix.Close(fd)
}
