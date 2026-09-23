package config

import (
	"testing"

	"github.com/yukkeorg/pachicounter2/pkg/signal"
)

func TestParseWiringBitRange(t *testing.T) {
	// hidpin はビット n が GPIO n に当たり、GPIO は 29 番まである。ポート値は 32 ビット。
	w, err := ParseWiring("start=29,bonus=22,densapo=31")
	if err != nil {
		t.Fatalf("上位のビットの配線がエラーになった: %v", err)
	}
	if w.Bits[signal.RoleStart] != 29 || w.Bits[signal.RoleBonus] != 22 || w.Bits[signal.RoleDensapo] != 31 {
		t.Errorf("配線 = %v", w.Bits)
	}

	for _, spec := range []string{"start=32", "start=-1"} {
		if _, err := ParseWiring(spec); err == nil {
			t.Errorf("%q を範囲内として受け付けた", spec)
		}
	}
}
