package service

import (
	"context"
	"testing"
	"time"

	"github.com/wyw14/solo-036/internal/model"
	"github.com/wyw14/solo-036/internal/tracker"
)

var testBaseTime = time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)

func launchTrackedRun(t *testing.T, runtime *Runtime) string {
	t.Helper()
	operation, err := runtime.Launch(context.Background(), LaunchRequest{
		StationID:      "station-a",
		RouteID:        "main-station-a-station-c",
		TargetPressure: 10,
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	runID := operation.Run.ID
	track(t, runtime, runID, "probe-a", 10, testBaseTime)
	track(t, runtime, runID, "probe-b", 20, testBaseTime.Add(time.Second))
	if _, err := runtime.GrantPassage(runID, "station-b"); err != nil {
		t.Fatalf("grant passage: %v", err)
	}
	return runID
}

func track(t *testing.T, runtime *Runtime, runID, source string, position float64, at time.Time) model.OperationRecord {
	t.Helper()
	operation, err := runtime.Track(runID, TrackingRequest{
		StationID:  requestStation(source),
		SourceID:   source,
		Kind:       tracker.SourceAcoustic,
		Position:   position,
		ObservedAt: at,
	})
	if err != nil {
		t.Fatalf("track %s: %v", source, err)
	}
	return operation
}

func requestStation(string) string { return "station-b" }

func observationID(operation model.OperationRecord, source string) string {
	for _, state := range operation.Tracking {
		if state.SourceID == source && state.ObservationID != "" {
			return state.ObservationID
		}
	}
	return ""
}

func openReview(operation model.OperationRecord) *model.RunReview {
	for i := range operation.Reviews {
		if operation.Reviews[i].Open() {
			return &operation.Reviews[i]
		}
	}
	return nil
}

func TestWithdrawWithoutCorrectionMarksPermitForReview(t *testing.T) {
	runtime := NewRuntime()
	runID := launchTrackedRun(t, runtime)
	operation, err := runtime.Operation(runID)
	if err != nil {
		t.Fatal(err)
	}
	obsA := observationID(operation, "probe-a")
	if obsA == "" {
		t.Fatal("missing probe-a observation id")
	}

	operation, err = runtime.WithdrawObservation(runID, WithdrawObservationRequest{
		StationID:     "station-b",
		ObservationID: obsA,
		Reason:        "probe number entered incorrectly",
	})
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}

	// Original observation is retained but marked withdrawn.
	history, err := runtime.ObservationHistory(runID, "station-b")
	if err != nil {
		t.Fatal(err)
	}
	var original *tracker.SourceObservation
	for i := range history {
		if history[i].ObservationID == obsA {
			original = &history[i]
		}
	}
	if original == nil || !original.Withdrawn {
		t.Fatal("withdrawn original observation must be retained in lineage")
	}

	if len(operation.Permits) != 1 || !operation.Permits[0].ReviewRequired {
		t.Fatalf("permit must be flagged for review: %+v", operation.Permits)
	}
	review := openReview(operation)
	if review == nil {
		t.Fatal("a review must be opened for the affected run decision")
	}
	for _, state := range operation.Tracking {
		if state.ObservationID == obsA && (!state.Withdrawn || !state.ReviewRequired || state.Online) {
			t.Fatalf("tracking state must reflect withdrawal: %+v", state)
		}
	}

	// Recomputed basis must fail on the single remaining observation.
	reassessment, err := runtime.ReassessPassage(runID, "station-b")
	if err != nil {
		t.Fatal(err)
	}
	if reassessment.EvidenceCount != 1 {
		t.Fatalf("expected 1 active evidence point, got %d", reassessment.EvidenceCount)
	}
	for _, basis := range reassessment.Basis {
		if basis.Passed {
			t.Fatalf("basis %s must fail after withdrawal without replacement", basis.Name)
		}
	}
	if !reassessment.ReviewOpen {
		t.Fatal("review must remain open while basis fails")
	}

	// A fresh observation at the same generation restores the basis, but the
	// decision still requires operator review.
	track(t, runtime, runID, "probe-c", 30, testBaseTime.Add(2*time.Second))
	reassessment, err = runtime.ReassessPassage(runID, "station-b")
	if err != nil {
		t.Fatal(err)
	}
	for _, basis := range reassessment.Basis {
		if !basis.Passed {
			t.Fatalf("basis %s must pass after replacement evidence", basis.Name)
		}
	}
	if !reassessment.ReviewOpen {
		t.Fatal("review must stay open until an operator resolves it")
	}
	if err := runtime.ResolveReview(runID, "station-b", "operator confirmed corrected evidence"); err != nil {
		t.Fatalf("resolve review: %v", err)
	}
	operation, _ = runtime.Operation(runID)
	if openReview(operation) != nil {
		t.Fatal("review must be closed after resolution")
	}
	for _, permit := range operation.Permits {
		if permit.ReviewRequired {
			t.Fatal("permit review flag must be cleared after resolution")
		}
	}
}

func TestWithdrawWithCorrectionPreservesRelationship(t *testing.T) {
	runtime := NewRuntime()
	runID := launchTrackedRun(t, runtime)
	operation, _ := runtime.Operation(runID)
	obsB := observationID(operation, "probe-b")

	operation, err := runtime.WithdrawObservation(runID, WithdrawObservationRequest{
		StationID:     "station-b",
		ObservationID: obsB,
		Reason:        "probe number entered incorrectly",
		Correction: &CorrectionInput{
			SourceID: "probe-c",
			Kind:     tracker.SourceAcoustic,
			Position: 20,
		},
	})
	if err != nil {
		t.Fatalf("withdraw with correction: %v", err)
	}

	corrections := runtime.Lineage.Corrections(runID)
	if len(corrections) != 1 {
		t.Fatalf("expected one correction record, got %d", len(corrections))
	}
	if corrections[0].Original.ObservationID != obsB || corrections[0].Correction.PhysicalSourceID != "probe-c" {
		t.Fatalf("correction relationship wrong: %+v", corrections[0])
	}
	if review := openReview(operation); review == nil {
		t.Fatal("corrected basis still requires review of the prior decision")
	}

	reassessment, err := runtime.ReassessPassage(runID, "station-b")
	if err != nil {
		t.Fatal(err)
	}
	for _, basis := range reassessment.Basis {
		if !basis.Passed {
			t.Fatalf("basis %s must pass after correction names a new probe", basis.Name)
		}
	}
}

func TestWithdrawRejectsUnknownOrCrossRunObservation(t *testing.T) {
	runtime := NewRuntime()
	runID := launchTrackedRun(t, runtime)

	if _, err := runtime.WithdrawObservation(runID, WithdrawObservationRequest{
		StationID:     "station-b",
		ObservationID: "does-not-exist",
		Reason:        "typo",
	}); err == nil {
		t.Fatal("withdraw of unknown observation must fail")
	}
	if _, err := runtime.WithdrawObservation(runID, WithdrawObservationRequest{
		StationID:     "station-b",
		ObservationID: "",
		Reason:        "typo",
	}); err == nil {
		t.Fatal("withdraw without observation id must fail")
	}
}

func TestSnapshotWithReviewsValidates(t *testing.T) {
	runtime := NewRuntime()
	runID := launchTrackedRun(t, runtime)
	operation, _ := runtime.Operation(runID)
	obsA := observationID(operation, "probe-a")
	if _, err := runtime.WithdrawObservation(runID, WithdrawObservationRequest{
		StationID:     "station-b",
		ObservationID: obsA,
		Reason:        "probe number entered incorrectly",
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Snapshot().Validate(); err != nil {
		t.Fatalf("snapshot including reviews must validate: %v", err)
	}
}

func TestRecoveryRebuildsWithdrawnLineageAndReview(t *testing.T) {
	directory := t.TempDir()
	runtime := NewRuntime()
	if err := runtime.ConfigurePersistence(directory); err != nil {
		t.Fatal(err)
	}
	runID := launchTrackedRun(t, runtime)
	operation, _ := runtime.Operation(runID)
	obsB := observationID(operation, "probe-b")
	if _, err := runtime.WithdrawObservation(runID, WithdrawObservationRequest{
		StationID:     "station-b",
		ObservationID: obsB,
		Reason:        "probe number entered incorrectly",
		Correction: &CorrectionInput{
			SourceID: "probe-c",
			Kind:     tracker.SourceAcoustic,
			Position: 20,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.PersistSnapshot(); err != nil {
		t.Fatalf("persist snapshot: %v", err)
	}

	recovered := NewRuntime()
	if err := recovered.ConfigurePersistence(directory); err != nil {
		t.Fatal(err)
	}
	if _, err := recovered.RecoverPersistent(); err != nil {
		t.Fatalf("recover: %v", err)
	}

	history, err := recovered.ObservationHistory(runID, "station-b")
	if err != nil {
		t.Fatal(err)
	}
	var originals, corrections int
	for _, observation := range history {
		if observation.ObservationID == obsB {
			if !observation.Withdrawn || observation.CorrectedBy == "" {
				t.Fatalf("withdrawn lineage not rebuilt: %+v", observation)
			}
			originals++
		}
		if observation.CorrectsObservationID == obsB {
			if observation.PhysicalSourceID != "probe-c" {
				t.Fatalf("correction rebuilt with wrong source: %+v", observation)
			}
			corrections++
		}
	}
	if originals != 1 || corrections != 1 {
		t.Fatalf("expected original and correction after recovery, got %d/%d", originals, corrections)
	}

	points := recovered.Lineage.Evidence(runID, "station-b", 1)
	if len(points) != 2 {
		t.Fatalf("expected 2 active evidence points after recovery, got %d: %+v", len(points), points)
	}
	reassessment, err := recovered.ReassessPassage(runID, "station-b")
	if err != nil {
		t.Fatal(err)
	}
	if !reassessment.ReviewOpen {
		t.Fatal("open review must survive recovery")
	}
	operation, _ = recovered.Operation(runID)
	if review := openReview(operation); review == nil {
		t.Fatal("review must be present after recovery")
	}
	if len(operation.Permits) != 1 || !operation.Permits[0].ReviewRequired {
		t.Fatalf("permit review flag must survive recovery: %+v", operation.Permits)
	}
}
