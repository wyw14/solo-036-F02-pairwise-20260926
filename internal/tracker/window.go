package tracker

import (
	"time"

	"github.com/wyw14/solo-036/internal/model"
)

// Continuous reports whether the evidence forms an unbroken chain: at least
// two points, all from the same generation, and no gap between adjacent
// observations (in observation order) larger than maxGap. It operates on the
// evidence it is given, so callers can recompute continuity after a source is
// withdrawn without touching the live coordinator.
func Continuous(points []model.PassageEvidence, maxGap time.Duration) bool {
	if len(points) < 2 {
		return false
	}
	for i := 1; i < len(points); i++ {
		if points[i].At.Sub(points[i-1].At) > maxGap {
			return false
		}
		if points[i].Generation != points[0].Generation {
			return false
		}
	}
	return true
}

func (c *Coordinator) Clearance(runID, station string, maxGap time.Duration) bool {
	return Continuous(c.Evidence(runID, station), maxGap)
}
