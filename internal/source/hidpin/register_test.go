package hidpin

import "testing"

func TestParseArg(t *testing.T) {
	cases := []struct {
		name    string
		arg     string
		want    string
		wantErr bool
	}{
		{name: "省略", arg: "", want: ""},
		{name: "シリアル番号", arg: "serial=E660583883693A2F", want: "E660583883693A2F"},
		{name: "前後の空白", arg: " serial = E660 ", want: "E660"},
		{name: "知らない設定", arg: "serail=E660", wantErr: true},
		{name: "key=value でない", arg: "E660", wantErr: true},
		{name: "シリアル番号が空", arg: "serial=", wantErr: true},
		{name: "二重指定", arg: "serial=a,serial=b", wantErr: true},
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
			if got.Serial != tc.want {
				t.Errorf("parseArg(%q).Serial = %q, 期待は %q", tc.arg, got.Serial, tc.want)
			}
		})
	}
}
