// Package vb は CR ウイルスブレイカーの機種プラグイン。
//
// 転落抽選型の確変機である。配線資料の電サポ端子の出力内容が
// 「大当り中、高ベース中及び転落抽選当選時の変動中に出力」であり、旧 Python 実装が
// 転落率 1/338.5 を定数として持っていたことがその裏付けになる。確変中は通常より
// 当たりやすいので、そこでの回転は初当たり確率の分母に入らない。旧実装が
// normalgames と chancegames を最初から分けていたのはこれが正しかったためである。
// 詳細は docs/adr/0004-per-machine-denominator-rule.md を参照。
//
// 出典: ~/Documents/Pachi/VIRUSBREAKER_OUTIF.pdf
package vb

import (
	"math"

	"github.com/yukkeorg/pachicounter2/pkg/machine"
	"github.com/yukkeorg/pachicounter2/pkg/signal"
)

const (
	// fallDenominator は転落抽選の分母。1 回転あたり 1/338.5 で転落する。
	fallDenominator = 338.5

	// startPayout はヘソ賞球数。この年代の CR 機の標準的な値を置いている。
	// 実機の仕様と違っていればここを直す。
	startPayout = 3

	// bonusPayoutAvg は 0 にしてある。この機種の平均出玉を確かな出典で
	// 確認できていないため、推定値を作らない。賞球信号を配線すれば獲得玉数は
	// 実測になるので、そちらで解決する。
	bonusPayoutAvg = 0
)

func init() {
	machine.Register(machine.Registration{
		ID:          "vb",
		DisplayName: "CR ウイルスブレイカー",
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

	// runRotations は今の確変区間に入ってからの回転数。旧実装が
	// chancegames として持ち、確変中の表示の分母に使っていた値である。
	// セッション累計の電サポ中回転数とは別物なので、プラグインの内部で持つ。
	runRotations int
}

func (p *plugin) inKakuhen() bool {
	return p.inDensapo && !p.inBonus
}

func (p *plugin) Spec() machine.Spec {
	return machine.Spec{
		ID:          "vb",
		DisplayName: "CR ウイルスブレイカー",
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

	if p.inDensapo {
		c.DensapoRotations++
		p.runRotations++
		return
	}
	c.NormalRotations++
}

func (p *plugin) onBonusStart(e machine.Edge, c *machine.Counters) {
	p.inBonus = true
	c.Bonuses++
	c.Chain++
	c.BonusHistory = append(c.BonusHistory, machine.BonusRecord{
		Chain:     c.Chain,
		Rotations: c.CurrentRotations,
		At:        e.At,
	})
}

func (p *plugin) onBonusEnd(c *machine.Counters) {
	p.inBonus = false
	c.CurrentRotations = 0
}

// onDensapoStart は電サポ信号の立上り、すなわち通常時から大当りに入った瞬間である。
// 連荘中の大当りではすでに立っているので、ここが初当たりの数え場所になる。
func (p *plugin) onDensapoStart(c *machine.Counters) {
	p.inDensapo = true
	c.FirstHits++
}

func (p *plugin) onDensapoEnd(c *machine.Counters) {
	p.inDensapo = false
	p.runRotations = 0
	c.Chain = 0
	c.CurrentRotations = 0
	c.BonusHistory = nil
}

func (p *plugin) State(c *machine.Counters) machine.State {
	switch {
	case p.inBonus:
		return machine.State{Kind: machine.StateBonus, Label: "大当り"}
	case p.inKakuhen():
		return machine.State{Kind: machine.StateKakuhen, Label: "確変"}
	default:
		return machine.State{Kind: machine.StateNormal, Label: "通常"}
	}
}

func (p *plugin) Metrics(c *machine.Counters) []machine.Metric {
	metrics := []machine.Metric{
		{
			Key:    "fall_denominator",
			Label:  "転落率",
			Value:  fallDenominator,
			Unit:   "1/x",
			Digits: 1,
		},
	}

	if p.inDensapo {
		metrics = append(metrics, machine.Metric{
			Key:   "kakuhen_rotations",
			Label: "確変中回転",
			Value: float64(p.runRotations),
			Unit:  "回転",
		})

		// 旧実装が確変中に表示していた「確変中回転 ÷ 連荘数」。
		// 確変中の当たりやすさを見る数字で、初当たり確率とは別物である。
		if c.Chain > 0 {
			metrics = append(metrics, machine.Metric{
				Key:    "kakuhen_rate",
				Label:  "確変中の当たり",
				Value:  float64(p.runRotations) / float64(c.Chain),
				Unit:   "1/x",
				Digits: 1,
			})
		}

		// 転落抽選を引かずにここまで続いている確率。旧実装にコメントとして
		// 残っていた計算で、今の連荘がどれだけ粘っているかの目安になる。
		metrics = append(metrics, machine.Metric{
			Key:    "fall_survival",
			Label:  "無転落継続率",
			Value:  math.Pow(1.0-1.0/fallDenominator, float64(p.runRotations)) * 100,
			Unit:   "%",
			Digits: 1,
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
