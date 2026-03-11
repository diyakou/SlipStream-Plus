package balancer

import (
	"math"
	"math/rand"

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

	bestLoad := int64(math.MaxInt64)
	best := make([]*engine.Instance, 0, len(instances))

	for _, inst := range instances {
		load := inst.ActiveConns()
		if load < bestLoad {
			bestLoad = load
			best = best[:0]
			best = append(best, inst)
			continue
		}
		if load == bestLoad {
			best = append(best, inst)
		}
	}

	if len(best) == 0 {
		return instances[0]
	}
	return best[rand.Intn(len(best))]
}
