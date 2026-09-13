package usbhid

import (
	"testing"
	"time"
)

func TestParseArg(t *testing.T) {
	cases := []struct {
		name    string
		arg     string
		want    Options
		wantErr bool
	}{
		{name: "省略", arg: "", want: Options{}},
		{name: "ドライバだけ", arg: "driver=usbio2", want: Options{Driver: "usbio2"}},
		{name: "両方", arg: "driver=usbio2, interval=10ms", want: Options{Driver: "usbio2", Interval: 10 * time.Millisecond}},
		{name: "知らない設定", arg: "drvier=usbio2", wantErr: true},
		{name: "key=value でない", arg: "usbio2", wantErr: true},
		{name: "ドライバが空", arg: "driver=", wantErr: true},
		{name: "間隔が読めない", arg: "interval=fast", wantErr: true},
		{name: "間隔が 0", arg: "interval=0s", wantErr: true},
		{name: "二重指定", arg: "driver=a,driver=b", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseArg(tc.arg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseArg(%q) がエラーにならず %+v を返した", tc.arg, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseArg(%q) がエラー: %v", tc.arg, err)
			}
			if got != tc.want {
				t.Errorf("parseArg(%q) = %+v, 期待は %+v", tc.arg, got, tc.want)
			}
		})
	}
}
