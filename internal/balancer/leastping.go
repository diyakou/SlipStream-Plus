package balancer

import (
	"math"

	"github.com/ParsaKSH/SlipStream-Plus/internal/engine"
)

type LeastPing struct{}

func NewLeastPing() *LeastPing {
	return &LeastPing{}
}

func (lp *LeastPing) Pick(instances []*engine.Instance) *engine.Instance {
	if len(instances) == 0 {
		return nil
	}

	best := instances[0]
	bestPing := best.LastPingMs()
	if bestPing <= 0 {
		bestPing = math.MaxInt64 - 1
	}

	for i := 1; i < len(instances); i++ {
		inst := instances[i]
		ping := inst.LastPingMs()
		if ping <= 0 {
			ping = math.MaxInt64 - 1
		}
		if ping < bestPing {
			bestPing = ping
			best = inst
		}
	}

	return best
}
