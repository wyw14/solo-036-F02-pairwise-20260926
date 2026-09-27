package tracker

import (
	"testing"
	"time"
)

func TestCoordinatorRetractRecomputesSources(t *testing.T) {
	coordinator := NewCoordinator()
	at := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	coordinator.Observe("run-1", "station-a", "probe-1/acoustic", "obs-1", 1, at)
	coordinator.Observe("run-1", "station-a", "probe-2/magnetic", "obs-2", 2, at.Add(20*time.Second))

	points := coordinator.Evidence("run-1", "station-a")
	if !coordinator.ArrivalQuorum(points, 2) {
		t.Fatal("expected quorum of two sources before retraction")
	}
	if !coordinator.Clearance("run-1", "station-a", 90*time.Second) {
		t.Fatal("expected clearance before retraction")
	}

	if !coordinator.Retract("obs-1") {
		t.Fatal("retract should find obs-1")
	}
	if coordinator.Retract("obs-1") {
		t.Fatal("second retract of the same observation must be a no-op")
	}

	points = coordinator.Evidence("run-1", "station-a")
	if coordinator.ArrivalQuorum(points, 2) {
		t.Fatal("quorum must fail after the observation is retracted")
	}
	if coordinator.Clearance("run-1", "station-a", 90*time.Second) {
		t.Fatal("a single remaining point cannot provide clearance")
	}
	if coordinator.Generation("run-1") != 1 {
		t.Fatal("retraction must not bump the generation")
	}
}

func TestCoordinatorRetractReopensContinuityGap(t *testing.T) {
	coordinator := NewCoordinator()
	at := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	coordinator.Observe("run-1", "station-a", "probe-1/acoustic", "obs-1", 1, at)
	coordinator.Observe("run-1", "station-a", "probe-2/pressure", "obs-2", 1, at.Add(40*time.Second))
	coordinator.Observe("run-1", "station-a", "probe-2/magnetic", "obs-3", 1, at.Add(120*time.Second))
	if !coordinator.Clearance("run-1", "station-a", 90*time.Second) {
		t.Fatal("40s and 80s gaps are both within the window")
	}
	coordinator.Retract("obs-2")
	if coordinator.Clearance("run-1", "station-a", 90*time.Second) {
		t.Fatal("removing the bridge point opens a 120s gap")
	}
}
