package tracker

import (
	"testing"
	"time"

	"github.com/wyw14/solo-036/internal/model"
)

func baseObservation(id, source string, at time.Time) SourceObservation {
	return SourceObservation{
		RunID:            "run-1",
		StationID:        "station-a",
		ObservationID:    id,
		PhysicalSourceID: source,
		Kind:             SourceAcoustic,
		Position:         1,
		Generation:       1,
		ObservedAt:       at,
		Valid:            true,
	}
}

func TestWithdrawPreservesOriginalAndRemovesFromBasis(t *testing.T) {
	lineage := NewSourceLineage()
	at := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	if _, err := lineage.Add(baseObservation("obs-1", "probe-1/acoustic", at)); err != nil {
		t.Fatalf("add obs-1: %v", err)
	}
	if _, err := lineage.Add(baseObservation("obs-2", "probe-2/magnetic", at.Add(20*time.Second))); err != nil {
		t.Fatalf("add obs-2: %v", err)
	}

	basis := lineage.Basis("run-1", "station-a", 1, 2, 90*time.Second)
	if !basis.Satisfied() {
		t.Fatalf("expected basis satisfied before withdrawal: %+v", basis)
	}

	withdrawn, ids, err := lineage.Withdraw("obs-1", "wrong probe number", at.Add(40*time.Second))
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if len(ids) != 1 || ids[0] != "obs-1" {
		t.Fatalf("unexpected withdrawn ids: %v", ids)
	}
	if withdrawn.Status != ObservationWithdrawn {
		t.Fatalf("withdrawn observation status = %q", withdrawn.Status)
	}
	if withdrawn.WithdrawReason != "wrong probe number" {
		t.Fatalf("withdraw reason = %q", withdrawn.WithdrawReason)
	}

	// The original observation is retained in history ...
	history := lineage.History("run-1", "station-a")
	if len(history) != 2 {
		t.Fatalf("history should retain both observations, got %d", len(history))
	}
	retained, ok := lineage.Get("obs-1")
	if !ok || retained.Status != ObservationWithdrawn {
		t.Fatalf("original observation not retained as withdrawn: %+v", retained)
	}

	// ... but no longer feeds the active evidence or the passage basis.
	if active := lineage.Observations("run-1", "station-a", 1); len(active) != 1 || active[0].ObservationID != "obs-2" {
		t.Fatalf("active observations = %v", active)
	}
	basis = lineage.Basis("run-1", "station-a", 1, 2, 90*time.Second)
	if basis.Independent {
		t.Fatalf("independence should fail after one physical source is withdrawn: %+v", basis.IndependentPhysicalSources)
	}
	if basis.Quorum {
		t.Fatalf("quorum should fail after withdrawal")
	}
	if basis.Satisfied() {
		t.Fatalf("basis must not be satisfied after withdrawal")
	}

	corrections := lineage.Corrections()
	if len(corrections) != 1 || corrections[0].PrimaryID != "obs-1" {
		t.Fatalf("withdrawal ledger = %+v", corrections)
	}
}

func TestWithdrawCascadesToDerivedObservations(t *testing.T) {
	lineage := NewSourceLineage()
	at := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	parent := baseObservation("parent", "probe-1/acoustic", at)
	if _, err := lineage.Add(parent); err != nil {
		t.Fatalf("add parent: %v", err)
	}
	child := baseObservation("child", "probe-1/acoustic", at.Add(5*time.Second))
	child.Kind = SourceDerivedPosition
	child.ParentObservationID = "parent"
	if _, err := lineage.Add(child); err != nil {
		t.Fatalf("add child: %v", err)
	}
	grandchild := baseObservation("grandchild", "probe-1/acoustic", at.Add(10*time.Second))
	grandchild.Kind = SourceDerivedPosition
	grandchild.ParentObservationID = "child"
	if _, err := lineage.Add(grandchild); err != nil {
		t.Fatalf("add grandchild: %v", err)
	}

	_, ids, err := lineage.Withdraw("parent", "probe retracted", at.Add(15*time.Second))
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	want := map[string]bool{"parent": true, "child": true, "grandchild": true}
	if len(ids) != 3 {
		t.Fatalf("expected parent + 2 cascaded ids, got %v", ids)
	}
	for _, id := range ids {
		if !want[id] {
			t.Fatalf("unexpected cascaded id %q", id)
		}
		observation, ok := lineage.Get(id)
		if !ok || observation.Status != ObservationWithdrawn {
			t.Fatalf("observation %s not withdrawn: %+v", id, observation)
		}
	}
	if active := lineage.Observations("run-1", "station-a", 1); len(active) != 0 {
		t.Fatalf("expected no active observations, got %v", active)
	}
}

func TestCorrectionKeepsOriginalAndRecordsRelationship(t *testing.T) {
	lineage := NewSourceLineage()
	at := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	if _, err := lineage.Add(baseObservation("obs-1", "probe-9/acoustic", at)); err != nil {
		t.Fatalf("add obs-1: %v", err)
	}
	if _, err := lineage.Add(baseObservation("obs-2", "probe-2/magnetic", at.Add(10*time.Second))); err != nil {
		t.Fatalf("add obs-2: %v", err)
	}

	correction := baseObservation("obs-3", "probe-1/acoustic", at.Add(20*time.Second))
	correction.CorrectsObservationID = "obs-1"
	record, err := lineage.Add(correction)
	if err != nil {
		t.Fatalf("add correction: %v", err)
	}
	if record == nil || record.PrimaryID != "obs-1" || record.ReplacementID != "obs-3" {
		t.Fatalf("correction record = %+v", record)
	}

	original, _ := lineage.Get("obs-1")
	if original.Status != ObservationWithdrawn {
		t.Fatalf("corrected original must be withdrawn, got %q", original.Status)
	}
	replacement, _ := lineage.Get("obs-3")
	if replacement.Status != ObservationActive || replacement.CorrectsObservationID != "obs-1" {
		t.Fatalf("replacement observation = %+v", replacement)
	}

	basis := lineage.Basis("run-1", "station-a", 1, 2, 90*time.Second)
	if !basis.Independent {
		t.Fatalf("probe-1 + probe-2 should be independent after correction: %+v", basis.IndependentPhysicalSources)
	}
	if !basis.Continuous {
		t.Fatalf("continuity should hold across the corrected observations")
	}
}

func TestCorrectionRejectsStaleTarget(t *testing.T) {
	lineage := NewSourceLineage()
	at := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	if _, err := lineage.Add(baseObservation("obs-1", "probe-9/acoustic", at)); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, _, err := lineage.Withdraw("obs-1", "retract", at.Add(time.Second)); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	correction := baseObservation("obs-2", "probe-1/acoustic", at.Add(2*time.Second))
	correction.CorrectsObservationID = "obs-1"
	if _, err := lineage.Add(correction); err == nil {
		t.Fatal("expected error correcting an already withdrawn observation")
	}
}

func TestBasisRecomputesContinuityAfterGap(t *testing.T) {
	lineage := NewSourceLineage()
	at := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	mustAdd := func(observation SourceObservation) {
		t.Helper()
		if _, err := lineage.Add(observation); err != nil {
			t.Fatalf("add %s: %v", observation.ObservationID, err)
		}
	}
	mustAdd(baseObservation("obs-1", "probe-1/acoustic", at))
	mustAdd(baseObservation("obs-2", "probe-2/magnetic", at.Add(30*time.Second)))
	mustAdd(baseObservation("obs-3", "probe-2/acoustic", at.Add(60*time.Second)))

	basis := lineage.Basis("run-1", "station-a", 1, 2, 90*time.Second)
	if !basis.Continuous {
		t.Fatal("expected continuity with three close points")
	}

	// Withdraw the bridge point in a sparse chain: the remaining endpoints
	// are 120s apart, beyond the 90s window.
	sparse := NewSourceLineage()
	mustAddSparse := func(id, source string, offset time.Duration) {
		t.Helper()
		if _, err := sparse.Add(baseObservation(id, source, at.Add(offset))); err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
	}
	mustAddSparse("s-1", "probe-1/acoustic", 0)
	mustAddSparse("s-2", "probe-2/magnetic", 40*time.Second)
	mustAddSparse("s-3", "probe-2/acoustic", 120*time.Second)
	if b := sparse.Basis("run-1", "station-a", 1, 2, 90*time.Second); !b.Continuous {
		t.Fatal("40s and 80s gaps are both within the 90s window before withdrawal")
	}
	if _, _, err := sparse.Withdraw("s-2", "bridge removed", at.Add(130*time.Second)); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	b := sparse.Basis("run-1", "station-a", 1, 2, 90*time.Second)
	if b.Continuous {
		t.Fatal("continuity must fail after the bridge point is withdrawn and a >90s gap opens")
	}
}

func TestGrantReviewFlaggedAfterWithdrawal(t *testing.T) {
	lineage := NewSourceLineage()
	at := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	if _, err := lineage.Add(baseObservation("obs-1", "probe-1/acoustic", at)); err != nil {
		t.Fatalf("add obs-1: %v", err)
	}
	if _, err := lineage.Add(baseObservation("obs-2", "probe-2/magnetic", at.Add(20*time.Second))); err != nil {
		t.Fatalf("add obs-2: %v", err)
	}
	review := lineage.RecordPassageGrant("run-1", "station-a", 1, "obs-2", 2, 90*time.Second, at.Add(30*time.Second))
	if review.ReviewRequired {
		t.Fatal("fresh grant must not require review")
	}

	if _, _, err := lineage.Withdraw("obs-1", "source withdrawn", at.Add(40*time.Second)); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	affected := lineage.MarkAffectedReviews("run-1", []string{"obs-1"}, "source withdrawn", 2, 90*time.Second)
	if len(affected) != 1 || !affected[0].ReviewRequired {
		t.Fatalf("affected reviews = %+v", affected)
	}
	if affected[0].Basis.Independent {
		t.Fatal("review basis should be recomputed without the withdrawn source")
	}
	if !lineage.ReviewRequired("run-1") {
		t.Fatal("run should be marked review required")
	}

	// A second withdrawal must not duplicate the flag.
	again := lineage.MarkAffectedReviews("run-1", []string{"obs-2"}, "again", 2, 90*time.Second)
	if len(again) != 0 {
		t.Fatalf("already flagged review should not be reported again: %+v", again)
	}
}

func TestContinuousAndIndependentHelpers(t *testing.T) {
	at := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	ev := func(source string, offset time.Duration, generation int64) model.PassageEvidence {
		return model.PassageEvidence{
			RunID: "run-1", Station: "station-a", SourceID: source,
			ObservationID: source, Generation: generation,
			At: at.Add(offset), Valid: true,
		}
	}
	if Continuous(nil, time.Minute) {
		t.Fatal("empty evidence is not continuous")
	}
	if Continuous([]model.PassageEvidence{ev("a", 0, 1)}, time.Minute) {
		t.Fatal("single point is not continuous")
	}
	if !Continuous([]model.PassageEvidence{ev("a", 0, 1), ev("b", 30*time.Second, 1)}, time.Minute) {
		t.Fatal("two close same-generation points are continuous")
	}
	if Continuous([]model.PassageEvidence{ev("a", 0, 1), ev("b", 2*time.Minute, 1)}, time.Minute) {
		t.Fatal("gap larger than maxGap breaks continuity")
	}
	if Continuous([]model.PassageEvidence{ev("a", 0, 1), ev("b", 30*time.Second, 2)}, time.Minute) {
		t.Fatal("generation change breaks continuity")
	}
	if !IndependentPhysicalArrival([]model.PassageEvidence{
		ev("probe-1/acoustic", 0, 1),
		ev("probe-1/magnetic", 10*time.Second, 1),
		ev("probe-2/pressure", 20*time.Second, 1),
	}, 2) {
		t.Fatal("two distinct physical prefixes should satisfy independence")
	}
	if IndependentPhysicalArrival([]model.PassageEvidence{
		ev("probe-1/acoustic", 0, 1),
		ev("probe-1/magnetic", 10*time.Second, 1),
	}, 2) {
		t.Fatal("channels of one physical probe are not independent")
	}
}
