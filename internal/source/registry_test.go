package source_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yukkeorg/pachicounter2/internal/source"

	_ "github.com/yukkeorg/pachicounter2/internal/source/replay"
	_ "github.com/yukkeorg/pachicounter2/internal/source/usbhid"
)

func writeLog(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "rec.jsonl")
	body := `{"type":"session_start","at_ns":0,"session":"s","machine":"stealth","wiring":{"start":0,"bonus":1,"densapo":2},"active_low":true}
{"type":"baseline","at_ns":0,"ports":15}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("記録を書けません: %v", err)
	}
	return path
}

func TestOpenRejectsUnknownSource(t *testing.T) {
	// dummy はテスト用のパッケージとしてだけ残し、名簿には載せない。
	for _, spec := range []string{"dummy", "nosuch:arg", ""} {
		if _, _, err := source.Open(spec, source.Env{}); err == nil {
			t.Errorf("%q を信号源として受け付けた", spec)
		}
	}
}

func TestOpenDeclaresKindAndRecording(t *testing.T) {
	path := writeLog(t)

	cases := []struct {
		spec          string
		wantName      string
		wantKind      source.Kind
		wantRecording bool
	}{
		{spec: "usbhid", wantName: "usbhid", wantKind: source.KindLive, wantRecording: false},
		{spec: "usbhid:driver=usbio2,interval=10ms", wantName: "usbhid", wantKind: source.KindLive, wantRecording: false},
		{spec: "file:" + path, wantName: "file", wantKind: source.KindReplay, wantRecording: true},
		{spec: "loop:" + path, wantName: "loop", wantKind: source.KindReplay, wantRecording: true},
	}

	for _, tc := range cases {
		t.Run(tc.wantName, func(t *testing.T) {
			src, reg, err := source.Open(tc.spec, source.Env{})
			if err != nil {
				t.Fatalf("%q を開けません: %v", tc.spec, err)
			}
			if reg.Name != tc.wantName {
				t.Errorf("名前 = %q, 期待は %q", reg.Name, tc.wantName)
			}
			if reg.Kind != tc.wantKind {
				t.Errorf("Kind = %v, 期待は %v", reg.Kind, tc.wantKind)
			}
			if _, ok := src.(source.Recording); ok != tc.wantRecording {
				t.Errorf("記録した設定を持つか = %v, 期待は %v", ok, tc.wantRecording)
			}
		})
	}
}

func TestOpenPassesArgumentErrors(t *testing.T) {
	for _, spec := range []string{"usbhid:drvier=usbio2", "file:", "loop:"} {
		if _, _, err := source.Open(spec, source.Env{}); err == nil {
			t.Errorf("%q を受け付けた", spec)
		}
	}
}
