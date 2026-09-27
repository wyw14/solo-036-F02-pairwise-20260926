package tracker

import (
	"sort"
	"time"

	"github.com/wyw14/solo-036/internal/model"
)

func (c *Coordinator) Observe(runID, station, source, observationID string, position float64, at time.Time) model.PassageEvidence {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation[runID] == 0 {
		c.generation[runID] = 1
	}
	e := model.PassageEvidence{
		RunID:         runID,
		Station:       station,
		SourceID:      source,
		ObservationID: observationID,
		Generation:    c.generation[runID],
		Position:      position,
		At:            at,
		Valid:         true,
	}
	key := runID + "/" + station
	c.points[key] = append(c.points[key], e)
	c.rebuildSources(key)
	return e
}

// Retract removes a single recorded observation without bumping the
// generation, so independence and continuity can be recomputed against the
// remaining observations of the same generation. It reports whether the
// observation was found.
func (c *Coordinator) Retract(observationID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, points := range c.points {
		for i, point := range points {
			if point.ObservationID != observationID {
				continue
			}
			c.points[key] = append(points[:i], points[i+1:]...)
			c.rebuildSources(key)
			return true
		}
	}
	return false
}

func (c *Coordinator) rebuildSources(key string) {
	sources := make(map[string]bool)
	for _, point := range c.points[key] {
		if model.ValidEvidence(point) {
			sources[point.SourceID] = true
		}
	}
	if len(sources) == 0 {
		delete(c.sources, key)
	} else {
		c.sources[key] = sources
	}
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
