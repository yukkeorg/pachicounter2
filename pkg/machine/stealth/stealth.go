// Package stealth は CR STEALTH block.III の機種プラグイン。
//
// 電サポ区間は SR（Stealth Rush）であり、大当り確率が通常より高い確変である。
// したがって SR 中の回転は初当たり確率の分母に入らない。旧 Python 実装が
// 確変終了時に何も加算していなかったのはこれが正しかったためで、
// 他機種と揃えて「統一」してはならない。詳細は
// docs/adr/0004-per-machine-denominator-rule.md を参照。
//
// スペック: 大当たり確率 1/155、SR 突入率 50%、SR 継続率 66%、15R・7C、賞球 3 & 10
// 出典: ~/Documents/Pachi/niconama_info.txt および STEALTH_OUTIF.pdf
package stealth

import (
	"github.com/yukkeorg/pachicounter2/pkg/machine"
	"github.com/yukkeorg/pachicounter2/pkg/signal"
)

const (
	// srRotations は SR の回転数。旧実装が SR 中に「{count} OF 99」と
	// 表示していた 99 がこれ。1 回当たるごとに数え直しになる。
	srRotations = 99

	// startPayout はヘソ賞球数。
	startPayout = 3

	// bonusPayoutAvg は大当り 1 回あたりの平均出玉の目安。
	//
	// 15R・7C から 15 × 7 × 賞球 10 個 = 1050 玉として置いた概算である。
	// 賞球信号を配線すれば獲得玉数は実測になるので、その場合この値は使われない。
	bonusPayoutAvg = 1050
)

func init() {
	machine.Register(machine.Registration{
		ID:          "stealth",
		DisplayName: "CR STEALTH block.III",
		New: func(string) (machine.Plugin, error) {
			return &plugin{}, nil
		},
	})
}

type plugin struct {
	// inBonus は大当り中かどうか。
	inBonus bool

	// inDensapo は電サポ信号が出ているかどうか。大当り中も出ている。
	inDensapo bool
}

// inRush は SR 中かどうかを返す。電サポ信号は大当り中も出ているので、
// 電サポが出ていて大当り中でなければ SR である。
func (p *plugin) inRush() bool {
	return p.inDensapo && !p.inBonus
}

func (p *plugin) Spec() machine.Spec {
	return machine.Spec{
		ID:          "stealth",
		DisplayName: "CR STEALTH block.III",
		RequiredRoles: []signal.Role{
			signal.RoleStart,
			signal.RoleBonus,
			signal.RoleDensapo,
		},
		OptionalRoles: []signal.Role{
			signal.RolePayoutBonus,
			signal.RolePayout,
		},
		StartPayout:    startPayout,
		BonusPayoutAvg: bonusPayoutAvg,
	}
}

func (p *plugin) Reset() {
	*p = plugin{}
}

func (p *plugin) Baseline(active machine.RoleSet) {
	p.inBonus = active.Has(signal.RoleBonus)
	p.inDensapo = active.Has(signal.RoleDensapo)
}

func (p *plugin) Edge(e machine.Edge, c *machine.Counters) {
	switch e.Role {
	case signal.RoleStart:
		if e.Rising {
			p.onRotation(c)
		}

	case signal.RoleBonus:
		if e.Rising {
			p.onBonusStart(e, c)
		} else {
			p.onBonusEnd(c)
		}

	case signal.RoleDensapo:
		if e.Rising {
			p.onDensapoStart(c)
		} else {
			p.onDensapoEnd(c)
		}
	}
}

func (p *plugin) onRotation(c *machine.Counters) {
	c.CurrentRotations++

	// SR 中は確変、つまり通常より当たりやすい状態で抽選されているので、
	// この回転を初当たり確率の分母に入れてはならない。
	if p.inDensapo {
		c.DensapoRotations++
		return
	}
	c.NormalRotations++
}

func (p *plugin) onBonusStart(e machine.Edge, c *machine.Counters) {
	p.inBonus = true
	c.Bonuses++

	// 連荘数はこの区間に入ってからの大当り回数。初当たりが 1 回目になる。
	c.Chain++
	c.BonusHistory = append(c.BonusHistory, machine.BonusRecord{
		Chain:     c.Chain,
		Rotations: c.CurrentRotations,
		At:        e.At,
	})
}

func (p *plugin) onBonusEnd(c *machine.Counters) {
	p.inBonus = false

	// 次の当たりまでの回転数を数え直す。SR の残り回転数もここを起点にする。
	c.CurrentRotations = 0
}

// onDensapoStart は電サポ信号の立上り。
//
// 電サポ信号は「大当り中または電サポ中」に出るので、これが立つのは通常時から
// 大当りに入った瞬間、つまり初当たりのときだけである。連荘中の大当りでは
// すでに立っているので立上りは起きない。よってここが初当たりの数え場所になる。
func (p *plugin) onDensapoStart(c *machine.Counters) {
	p.inDensapo = true
	c.FirstHits++
}

func (p *plugin) onDensapoEnd(c *machine.Counters) {
	p.inDensapo = false
	c.Chain = 0
	c.CurrentRotations = 0
	c.BonusHistory = nil
}

func (p *plugin) State(c *machine.Counters) machine.State {
	switch {
	case p.inBonus:
		return machine.State{Kind: machine.StateBonus, Label: "大当り"}
	case p.inRush():
		// SR は確率変動なので確変として扱う。時短ではない。
		return machine.State{Kind: machine.StateKakuhen, Label: "STEALTH RUSH"}
	default:
		return machine.State{Kind: machine.StateNormal, Label: "通常"}
	}
}

func (p *plugin) Metrics(c *machine.Counters) []machine.Metric {
	metrics := []machine.Metric{}

	if p.inRush() {
		remain := srRotations - c.CurrentRotations
		if remain < 0 {
			remain = 0
		}
		metrics = append(metrics, machine.Metric{
			Key:   "sr_remain",
			Label: "SR残",
			Value: float64(remain),
			Unit:  "回転",
		})
	}

	if c.PayoutPulses > 0 {
		metrics = append(metrics, machine.Metric{
			Key:   "payout_pulses",
			Label: "賞球パルス",
			Value: float64(c.PayoutPulses),
			Unit:  "回",
		})
	}

	return metrics
}
