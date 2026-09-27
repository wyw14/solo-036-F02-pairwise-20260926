package tracker

import (
	"testing"
	"time"

	"github.com/wyw14/solo-036/internal/model"
)

func testObservation(id, source string, gen int64, at time.Time) SourceObservation {
	return SourceObservation{
		RunID:            "run-1",
		StationID:        "station-b",
		ObservationID:    id,
		PhysicalSourceID: source,
		Kind:             SourceMagnetic,
		Position:         10,
		Generation:       gen,
		ObservedAt:       at,
		Valid:            true,
	}
}

func TestWithdrawKeepsOriginalAndLinksCorrection(t *testing.T) {
	lineage := NewSourceLineage()
	t0 := time.Now().UTC()
	original := testObservation("obs-1", "probe-a", 1, t0)
	if err := lineage.Add(original); err != nil {
		t.Fatalf("add original: %v", err)
	}

	changed, err := lineage.Withdraw("obs-1", "probe id mistyped", "obs-2", t0.Add(time.Second))
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if len(changed) != 1 {
		t.Fatalf("expected 1 changed observation, got %d", len(changed))
	}

	// Original remains in the lineage and is marked withdrawn.
	kept, ok := lineage.Observation("obs-1")
	if !ok {
		t.Fatal("original observation was deleted from lineage")
	}
	if !kept.Withdrawn || kept.CorrectedBy != "obs-2" {
		t.Fatalf("original not marked withdrawn/linked: %+v", kept)
	}

	correction := testObservation("obs-2", "probe-b", 1, t0.Add(time.Second))
	correction.CorrectsObservationID = "obs-1"
	correction.WithdrawReason = "probe id mistyped"
	if err := lineage.Add(correction); err != nil {
		t.Fatalf("add correction: %v", err)
	}

	if points := lineage.Evidence("run-1", "station-b", 1); len(points) != 1 {
		t.Fatalf("expected only the correction in active evidence, got %d", len(points))
	} else if points[0].SourceID != "probe-b" {
		t.Fatalf("active evidence has wrong source %q", points[0].SourceID)
	}

	corrections := lineage.Corrections("run-1")
	if len(corrections) != 1 || corrections[0].Original.ObservationID != "obs-1" ||
		corrections[0].Correction.ObservationID != "obs-2" {
		t.Fatalf("correction relationship not preserved: %+v", corrections)
	}
}

func TestWithdrawCascadesToDerivedObservations(t *testing.T) {
	lineage := NewSourceLineage()
	t0 := time.Now().UTC()
	base := testObservation("obs-1", "probe-a", 1, t0)
	derived := testObservation("obs-2", "probe-a", 1, t0.Add(time.Second))
	derived.ParentObservationID = "obs-1"
	derived.Kind = SourceDerivedPosition
	if err := lineage.Add(base); err != nil {
		t.Fatal(err)
	}
	if err := lineage.Add(derived); err != nil {
		t.Fatal(err)
	}

	changed, err := lineage.Withdraw("obs-1", "bad probe", "", t0.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 2 {
		t.Fatalf("expected withdrawal to cascade to derived observation, got %d changed", len(changed))
	}
	if points := lineage.Evidence("run-1", "station-b", 1); len(points) != 0 {
		t.Fatalf("expected no active evidence after cascade withdrawal, got %d", len(points))
	}
}

func TestRecomputeIndependenceAfterCorrection(t *testing.T) {
	lineage := NewSourceLineage()
	t0 := time.Now().UTC()
	_ = lineage.Add(testObservation("obs-1", "probe-a", 1, t0))
	_ = lineage.Add(testObservation("obs-2", "probe-a/secondary", 1, t0.Add(time.Second)))

	// Same physical probe ("probe-a") -> not independent.
	if IndependentPhysicalArrival(lineage.Evidence("run-1", "station-b", 1), 2) {
		t.Fatal("two channels of one probe must not count as independent")
	}

	_, _ = lineage.Withdraw("obs-2", "wrong probe entered", "obs-3", t0.Add(2*time.Second))
	correction := testObservation("obs-3", "probe-b", 1, t0.Add(2*time.Second))
	correction.CorrectsObservationID = "obs-2"
	if err := lineage.Add(correction); err != nil {
		t.Fatal(err)
	}

	points := lineage.Evidence("run-1", "station-b", 1)
	if !IndependentPhysicalArrival(points, 2) {
		t.Fatal("independence must hold after the correction names a second probe")
	}
	if !Continuous(points, 90*time.Second) {
		t.Fatal("continuity must hold within the gap window")
	}
}

func TestContinuityFailsWhenCorrectionLeavesGap(t *testing.T) {
	lineage := NewSourceLineage()
	t0 := time.Now().UTC()
	_ = lineage.Add(testObservation("obs-1", "probe-a", 1, t0))
	mid := testObservation("obs-2", "probe-b", 1, t0.Add(60*time.Second))
	_ = lineage.Add(mid)
	_ = lineage.Add(testObservation("obs-3", "probe-a", 1, t0.Add(120*time.Second)))

	if !Continuous(lineage.Evidence("run-1", "station-b", 1), 90*time.Second) {
		t.Fatal("continuity should hold before withdrawal")
	}
	_, _ = lineage.Withdraw("obs-2", "wrong probe", "", t0.Add(180*time.Second))
	if Continuous(lineage.Evidence("run-1", "station-b", 1), 90*time.Second) {
		t.Fatal("continuity must fail once the bridging observation is withdrawn")
	}
}

func TestCoordinatorRetractRemovesOnlyWithdrawnPoints(t *testing.T) {
	coordinator := NewCoordinator()
	t0 := time.Now().UTC()
	a := coordinator.ObserveWithID("obs-1", "run-1", "station-b", "probe-a", 1, t0)
	_ = coordinator.ObserveWithID("obs-2", "run-1", "station-b", "probe-b", 2, t0.Add(time.Second))

	removed := coordinator.Retract(SourceObservation{ObservationID: a.ObservationID})
	if removed != 1 {
		t.Fatalf("expected 1 retracted point, got %d", removed)
	}
	points := coordinator.Evidence("run-1", "station-b")
	if len(points) != 1 || points[0].ObservationID != "obs-2" {
		t.Fatalf("unexpected remaining points: %+v", points)
	}
}

func TestPassageEvidenceCarriesObservationID(t *testing.T) {
	observation := testObservation("obs-9", "probe-a", 1, time.Now().UTC())
	evidence := PassageEvidence(observation)
	if evidence.ObservationID != "obs-9" || evidence.SourceID != "probe-a" {
		t.Fatalf("evidence mapping lost identity: %+v", evidence)
	}
	var _ model.PassageEvidence = evidence
}
