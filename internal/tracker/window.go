package tracker

import "time"

// Clearance reports whether the coordinator's current points at the station
// form an unbroken observation chain. Prefer tracker.Continuous over a
// lineage-derived evidence set when reassessing after a withdrawal.
func (c *Coordinator) Clearance(runID, station string, maxGap time.Duration) bool {
	return Continuous(c.Evidence(runID, station), maxGap)
}
