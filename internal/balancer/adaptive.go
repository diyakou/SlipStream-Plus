package balancer

import (
	"math"
	"math/rand"

	"github.com/ParsaKSH/SlipStream-Plus/internal/engine"
)

// Adaptive prefers low-load instances and uses latency as a secondary signal.
type Adaptive struct{}

func NewAdaptive() *Adaptive {
	return &Adaptive{}
}

func (a *Adaptive) Pick(instances []*engine.Instance) *engine.Instance {
	if len(instances) == 0 {
		return nil
	}

	bestScore := int64(math.MaxInt64)
	best := make([]*engine.Instance, 0, len(instances))

	for _, inst := range instances {
		ping := inst.LastPingMs()
		if ping <= 0 {
			ping = 5000
		}

		// Load is weighted higher so hot instances are avoided quickly.
		score := inst.ActiveConns()*1000 + ping
		if score < bestScore {
			bestScore = score
			best = best[:0]
			best = append(best, inst)
			continue
		}
		if score == bestScore {
			best = append(best, inst)
		}
	}

	if len(best) == 0 {
		return instances[0]
	}
	return best[rand.Intn(len(best))]
}
