package balancer

import (
	"math"

	"github.com/ParsaKSH/SlipStream-Plus/internal/engine"
)

type LeastLoad struct{}

func NewLeastLoad() *LeastLoad {
	return &LeastLoad{}
}

func (ll *LeastLoad) Pick(instances []*engine.Instance) *engine.Instance {
	if len(instances) == 0 {
		return nil
	}

	best := instances[0]
	bestLoad := best.ActiveConns()

	for i := 1; i < len(instances); i++ {
		inst := instances[i]
		load := inst.ActiveConns()
		if load < bestLoad {
			bestLoad = load
			best = inst
		}
	}

	return best
}
