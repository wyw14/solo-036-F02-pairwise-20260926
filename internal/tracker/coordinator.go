package tracker

import (
	"sort"
	"time"

	"github.com/wyw14/solo-036/internal/model"
)

func (c *Coordinator) Observe(runID, station, source string, position float64, at time.Time) model.PassageEvidence {
	return c.ObserveWithID(model.NewRevision(), runID, station, source, position, at)
}

// ObserveWithID records an evidence point carrying the lineage observation id,
// so a later withdrawal can retract exactly this point.
func (c *Coordinator) ObserveWithID(observationID, runID, station, source string, position float64, at time.Time) model.PassageEvidence {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation[runID] == 0 {
		c.generation[runID] = 1
	}
	e := model.PassageEvidence{ObservationID: observationID, RunID: runID, Station: station, SourceID: source, Generation: c.generation[runID], Position: position, At: at, Valid: true}
	key := runID + "/" + station
	c.points[key] = append(c.points[key], e)
	if c.sources[key] == nil {
		c.sources[key] = make(map[string]bool)
	}
	c.sources[key][source] = true
	return e
}

// Retract removes the evidence points recorded under the withdrawn
// observations. It reports how many points were removed.
func (c *Coordinator) Retract(observations ...SourceObservation) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	withdrawn := make(map[string]bool, len(observations))
	for _, observation := range observations {
		withdrawn[observation.ObservationID] = true
	}
	removed := 0
	for key, points := range c.points {
		kept := points[:0]
		for _, point := range points {
			if withdrawn[point.ObservationID] {
				removed++
				continue
			}
			kept = append(kept, point)
		}
		c.points[key] = kept
	}
	for key, points := range c.points {
		sources := make(map[string]bool)
		for _, point := range points {
			sources[point.SourceID] = true
		}
		c.sources[key] = sources
	}
	return removed
}

func (c *Coordinator) Revise(runID string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation[runID]++
	for key := range c.points {
		if len(key) >= len(runID) && key[:len(runID)] == runID {
			delete(c.points, key)
			delete(c.sources, key)
		}
	}
	return c.generation[runID]
}
func (c *Coordinator) Generation(runID string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation[runID] == 0 {
		c.generation[runID] = 1
	}
	return c.generation[runID]
}
func (c *Coordinator) Evidence(runID, station string) []model.PassageEvidence {
	c.mu.Lock()
	defer c.mu.Unlock()
	in := c.points[runID+"/"+station]
	out := append([]model.PassageEvidence(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].At.Equal(out[j].At) {
			return out[i].ObservationID < out[j].ObservationID
		}
		return out[i].At.Before(out[j].At)
	})
	return out
}
