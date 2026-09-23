package signal

import "testing"

func TestPortsBit(t *testing.T) {
	p := Ports(1<<31 | 1<<29 | 1)
	for n, want := range map[int]bool{0: true, 1: false, 29: true, 31: true, 32: false, -1: false} {
		if got := p.Bit(n); got != want {
			t.Errorf("Bit(%d) = %v, 期待は %v", n, got, want)
		}
	}
}
