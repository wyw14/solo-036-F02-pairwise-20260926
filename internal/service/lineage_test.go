package service

import (
	"context"
	"testing"
	"time"
)

func launchRun(t *testing.T, runtime *Runtime) string {
	t.Helper()
	operation, err := runtime.Launch(context.Background(), LaunchRequest{
		StationID:      "station-a",
		RouteID:        "main-station-a-station-c",
		TargetPressure: 8.0,
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	return operation.Run.ID
}

func trackAt(runtime *Runtime, runID, source string, position float64, at time.Time, corrects string) (string, error) {
	operation, err := runtime.Track(runID, TrackingRequest{
		StationID:             "station-a",
		SourceID:              source,
		Kind:                  "acoustic",
		Position:              position,
		ObservedAt:            at,
		CorrectsObservationID: corrects,
	})
	if err != nil {
		return "", err
	}
	tracking := operation.Tracking
	return tracking[len(tracking)-1].ObservationID, nil
}

func TestWithdrawObservationFlagsPassagePermitForReview(t *testing.T) {
	runtime := NewRuntime()
	runID := launchRun(t, runtime)
	base := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)

	obs1, err := trackAt(runtime, runID, "probe-1/acoustic", 1, base, "")
	if err != nil {
		t.Fatalf("track obs-1: %v", err)
	}
	if _, err := trackAt(runtime, runID, "probe-2/magnetic", 2, base.Add(20*time.Second), ""); err != nil {
		t.Fatalf("track obs-2: %v", err)
	}

	if _, err := runtime.GrantPassage(runID, "station-a"); err != nil {
		t.Fatalf("grant passage: %v", err)
	}
	operation, _ := runtime.Operation(runID)
	if len(operation.Permits) != 1 || operation.Permits[0].ReviewRequired {
		t.Fatalf("fresh permit should be granted without review: %+v", operation.Permits)
	}

	report, err := runtime.WithdrawObservation(runID, WithdrawObservationRequest{
		ObservationID: obs1,
		Reason:        "probe number mistyped",
	})
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}

	// Original observation retained, marked withdrawn, correction recorded.
	if len(report.Observations) != 2 {
		t.Fatalf("expected both observations retained, got %d", len(report.Observations))
	}
	var withdrawnFound bool
	for _, observation := range report.Observations {
		if observation.ObservationID == obs1 {
			withdrawnFound = observation.Status == "withdrawn"
		}
	}
	if !withdrawnFound {
		t.Fatal("withdrawn observation not retained in lineage history")
	}
	if len(report.Corrections) != 1 || report.Corrections[0].PrimaryID != obs1 {
		t.Fatalf("correction ledger = %+v", report.Corrections)
	}

	// Basis recomputed: one physical source left -> not independent / quorum.
	if len(report.Bases) != 1 {
		t.Fatalf("expected one basis, got %d", len(report.Bases))
	}
	if report.Bases[0].Independent || report.Bases[0].Quorum {
		t.Fatalf("basis must fail independence after withdrawal: %+v", report.Bases[0])
	}
	if !report.ReviewRequired {
		t.Fatal("run must be marked review required")
	}
	if len(report.Reviews) != 1 || !report.Reviews[0].ReviewRequired {
		t.Fatalf("passage review not flagged: %+v", report.Reviews)
	}

	// The granted permit is flagged for review but not revoked.
	operation, _ = runtime.Operation(runID)
	permit := operation.Permits[0]
	if !permit.Granted || !permit.ReviewRequired {
		t.Fatalf("permit not flagged for review: %+v", permit)
	}
	if permit.ReviewReason == "" {
		t.Fatal("review reason should explain the withdrawal")
	}

	// Generation is unchanged and a withdrawal event was journaled.
	if generation := runtime.Tracker.Generation(runID); generation != 1 {
		t.Fatalf("withdrawal must not bump generation, got %d", generation)
	}
	var journaled bool
	for _, event := range runtime.Journal.Events() {
		if event.Kind == "tracking_withdrawal" && event.RunID == runID {
			journaled = true
		}
	}
	if !journaled {
		t.Fatal("withdrawal not recorded in journal")
	}
}

func TestCorrectObservationRecomputesPassageBasis(t *testing.T) {
	runtime := NewRuntime()
	runID := launchRun(t, runtime)
	base := time.Date(2026, 9, 26, 11, 0, 0, 0, time.UTC)

	// Operator enters probe-9 by mistake ...
	wrongID, err := trackAt(runtime, runID, "probe-9/acoustic", 1, base, "")
	if err != nil {
		t.Fatalf("track wrong: %v", err)
	}
	// ... then records the correction with the real probe number, plus an
	// independent second source.
	if _, err := trackAt(runtime, runID, "probe-1/magnetic", 2, base.Add(10*time.Second), wrongID); err != nil {
		t.Fatalf("track correction: %v", err)
	}
	if _, err := trackAt(runtime, runID, "probe-2/pressure", 3, base.Add(20*time.Second), ""); err != nil {
		t.Fatalf("track second source: %v", err)
	}

	if _, err := runtime.GrantPassage(runID, "station-a"); err != nil {
		t.Fatalf("grant passage after correction: %v", err)
	}

	report, err := runtime.LineageReport(runID)
	if err != nil {
		t.Fatalf("lineage report: %v", err)
	}
	var activeSources []string
	for _, observation := range report.Observations {
		if observation.Status == "active" {
			activeSources = append(activeSources, observation.PhysicalSourceID)
		}
	}
	if len(activeSources) != 2 {
		t.Fatalf("expected two active observations after correction, got %v", activeSources)
	}
	if len(report.Corrections) != 1 || report.Corrections[0].ReplacementID == "" {
		t.Fatalf("correction relationship missing: %+v", report.Corrections)
	}
	if !report.Bases[0].Independent || !report.Bases[0].Continuous {
		t.Fatalf("basis after correction should hold: %+v", report.Bases[0])
	}
}

func TestWithdrawRejectsUnknownAndCrossRunObservation(t *testing.T) {
	runtime := NewRuntime()
	runID := launchRun(t, runtime)
	// A second run record without reserving the single shared route.
	other := runtime.Runs.Create("station-a")
	base := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	obsID, err := trackAt(runtime, runID, "probe-1/acoustic", 1, base, "")
	if err != nil {
		t.Fatalf("track: %v", err)
	}
	if _, err := runtime.WithdrawObservation(other.ID, WithdrawObservationRequest{ObservationID: obsID}); err == nil {
		t.Fatal("expected cross-run withdrawal to be rejected")
	}
	if _, err := runtime.WithdrawObservation(runID, WithdrawObservationRequest{ObservationID: "does-not-exist"}); err == nil {
		t.Fatal("expected unknown observation withdrawal to be rejected")
	}
}
