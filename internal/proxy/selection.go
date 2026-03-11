package proxy

import "github.com/ParsaKSH/SlipStream-Plus/internal/engine"

func removeInstanceByID(instances []*engine.Instance, id int) []*engine.Instance {
	for i, inst := range instances {
		if inst.ID() == id {
			return append(instances[:i], instances[i+1:]...)
		}
	}
	return instances
}
