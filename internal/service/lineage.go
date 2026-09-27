package service

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wyw14/solo-036/internal/model"
	"github.com/wyw14/solo-036/internal/tracker"
)

// PassageMaxGap bounds the acceptable time gap inside a continuous passage
// observation chain.
const PassageMaxGap = 90 * time.Second

// CorrectionInput re-records an observation after a probe id entry error.
type CorrectionInput struct {
	SourceID   string             `json:"source_id"`
	Kind       tracker.SourceKind `json:"kind"`
	Position   float64            `json:"position"`
	ObservedAt time.Time          `json:"observed_at"`
}

// WithdrawObservationRequest withdraws a mistyped observation. Correction is
// optional: when present the corrected observation is recorded under the same
// generation and linked to the original.
type WithdrawObservationRequest struct {
	StationID     string           `json:"station_id"`
	ObservationID string           `json:"observation_id"`
	Reason        string           `json:"reason"`
	Correction    *CorrectionInput `json:"correction,omitempty"`
}

// BasisResult is one recomputed passage dimension after a withdrawal.
type BasisResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// PassageReassessment reports the recomputed passage basis for a station.
type PassageReassessment struct {
	RunID         string        `json:"run_id"`
	StationID     string        `json:"station_id"`
	Generation    int64         `json:"generation"`
	EvidenceCount int           `json:"evidence_count"`
	Basis         []BasisResult `json:"basis"`
	ReviewOpen    bool          `json:"review_open"`
}

func (r *Runtime) observation(runID, stationID, observationID string) (tracker.SourceObservation, error) {
	if strings.TrimSpace(observationID) == "" {
		return tracker.SourceObservation{}, errors.New("observation id is required")
	}
	observation, ok := r.Lineage.Observation(observationID)
	if !ok {
		return tracker.SourceObservation{}, fmt.Errorf("observation %s not found", observationID)
	}
	if observation.RunID != runID {
		return tracker.SourceObservation{}, fmt.Errorf("observation %s belongs to another run", observationID)
	}
	if stationID != "" && observation.StationID != stationID {
		return tracker.SourceObservation{}, fmt.Errorf("observation %s belongs to another station", observationID)
	}
	return observation, nil
}

// WithdrawObservation preserves the original observation, records the
// correction relationship, retracts the withdrawn evidence points and marks
// every run decision derived from that evidence as requiring review.
func (r *Runtime) WithdrawObservation(runID string, request WithdrawObservationRequest) (model.OperationRecord, error) {
	if _, ok := r.Runs.Get(runID); !ok {
		return model.OperationRecord{}, fmt.Errorf("run %s not found", runID)
	}
	if strings.TrimSpace(request.Reason) == "" {
		return model.OperationRecord{}, errors.New("withdrawal reason is required")
	}
	target, err := r.observation(runID, request.StationID, request.ObservationID)
	if err != nil {
		return model.OperationRecord{}, err
	}
	if target.Withdrawn {
		return model.OperationRecord{}, errors.New("observation is already withdrawn")
	}
	now := time.Now().UTC()

	correctionID := ""
	var correction *tracker.SourceObservation
	if request.Correction != nil {
		if strings.TrimSpace(request.Correction.SourceID) == "" {
			return model.OperationRecord{}, errors.New("correction source is required")
		}
		if request.Correction.SourceID == target.PhysicalSourceID && request.Correction.Position == target.Position {
			return model.OperationRecord{}, errors.New("correction must change the recorded probe source")
		}
		kind := request.Correction.Kind
		if kind == "" {
			kind = target.Kind
		}
		observedAt := request.Correction.ObservedAt
		if observedAt.IsZero() {
			observedAt = target.ObservedAt
		}
		correctionID = model.NewRevision()
		correction = &tracker.SourceObservation{
			RunID:                 runID,
			StationID:             target.StationID,
			ObservationID:         correctionID,
			PhysicalSourceID:      request.Correction.SourceID,
			CorrectsObservationID: target.ObservationID,
			Kind:                  kind,
			Position:              request.Correction.Position,
			Generation:            target.Generation,
			ObservedAt:            observedAt,
			Valid:                 true,
			WithdrawReason:        request.Reason,
		}
	}

	withdrawn, err := r.Lineage.Withdraw(target.ObservationID, request.Reason, correctionID, now)
	if err != nil {
		return model.OperationRecord{}, err
	}
	r.Tracker.Retract(withdrawn...)

	if correction != nil {
		if err := r.Lineage.Add(*correction); err != nil {
			return model.OperationRecord{}, err
		}
		r.Tracker.ObserveWithID(correction.ObservationID, runID, correction.StationID,
			correction.PhysicalSourceID, correction.Position, correction.ObservedAt)
		r.appendCorrectionTracking(*correction, request.Reason)
	}

	withdrawnIDs := make([]string, 0, len(withdrawn))
	for _, observation := range withdrawn {
		withdrawnIDs = append(withdrawnIDs, observation.ObservationID)
	}
	r.markWithdrawnTracking(runID, withdrawnIDs, request.Reason)
	r.openPassageReview(runID, target.StationID, target.Generation, request.Reason, withdrawnIDs)

	payload := target.StationID + "/" + target.ObservationID
	if correctionID != "" {
		payload += "/corrected-by/" + correctionID
	}
	if err := r.appendEvent(model.Event{
		RunID:   runID,
		Kind:    "tracking_withdrawal",
		Epoch:   target.Generation,
		Payload: payload,
		At:      now,
	}); err != nil {
		return model.OperationRecord{}, err
	}
	return r.Operation(runID)
}

// markWithdrawnTracking flags the stored tracking states of retracted
// observations; the historical rows are kept for the audit trail.
func (r *Runtime) markWithdrawnTracking(runID string, ids []string, reason string) {
	index := make(map[string]bool, len(ids))
	for _, id := range ids {
		index[id] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	states := r.control.Tracking[runID]
	for i := range states {
		if index[states[i].ObservationID] {
			states[i].Withdrawn = true
			states[i].WithdrawReason = reason
			states[i].Online = false
			states[i].ReviewRequired = true
		}
	}
	r.control.Tracking[runID] = states
	r.control.Revision = model.NewRevision()
}

// appendCorrectionTracking stores the corrective observation so the snapshot
// can reconstruct the full lineage after recovery.
func (r *Runtime) appendCorrectionTracking(observation tracker.SourceObservation, reason string) {
	state := model.TrackingState{
		RunID:                 observation.RunID,
		StationID:             observation.StationID,
		SourceID:              observation.PhysicalSourceID,
		ObservationID:         observation.ObservationID,
		CorrectsObservationID: observation.CorrectsObservationID,
		Kind:                  string(observation.Kind),
		Generation:            observation.Generation,
		Position:              observation.Position,
		Online:                true,
		WithdrawReason:        reason,
		ObservedAt:            observation.ObservedAt,
	}
	r.mu.Lock()
	r.control.Tracking[observation.RunID] = append(r.control.Tracking[observation.RunID], state)
	r.control.Revision = model.NewRevision()
	r.mu.Unlock()
}

// openPassageReview marks every permit granted at the affected generation as
// needing review and returns the open review's failing basis dimensions.
func (r *Runtime) openPassageReview(runID, stationID string, generation int64, reason string, withdrawnIDs []string) []string {
	r.mu.Lock()
	affected := false
	permits := r.control.Permits[runID]
	for i := range permits {
		if permits[i].Generation == generation {
			permits[i].ReviewRequired = true
			affected = true
		}
	}
	r.control.Permits[runID] = permits
	if affected {
		var existing *model.RunReview
		for i := range r.control.Reviews {
			review := &r.control.Reviews[i]
			if review.RunID == runID && review.StationID == stationID && review.Generation == generation && review.Open() {
				existing = review
			}
		}
		if existing == nil {
			r.control.Reviews = append(r.control.Reviews, model.RunReview{
				RunID:         runID,
				StationID:     stationID,
				Generation:    generation,
				Reason:        reason,
				AffectedBasis: []string{model.BasisIndependence, model.BasisContinuity},
				Withdrawn:     append([]string(nil), withdrawnIDs...),
				OpenedAt:      time.Now().UTC(),
			})
		}
	}
	r.control.Revision = model.NewRevision()
	r.mu.Unlock()

	// Compute the failing basis outside the control lock using lineage state.
	reassessment := r.recomputeBasis(runID, stationID, generation)
	failing := make([]string, 0, len(reassessment.Basis))
	for _, basis := range reassessment.Basis {
		if !basis.Passed {
			failing = append(failing, basis.Name)
		}
	}
	r.mu.Lock()
	for i := range r.control.Reviews {
		review := &r.control.Reviews[i]
		if review.RunID == runID && review.StationID == stationID && review.Generation == generation && review.Open() {
			review.AffectedBasis = failing
			if len(failing) == 0 {
				review.AffectedBasis = []string{model.BasisIndependence, model.BasisContinuity}
			}
		}
	}
	r.control.Revision = model.NewRevision()
	r.mu.Unlock()
	return failing
}

// ReassessPassage recomputes independence, continuity and quorum from the
// active lineage evidence. The review itself stays open even when every
// dimension holds: an operator confirms the recomputed basis through
// ResolveReview.
func (r *Runtime) ReassessPassage(runID, stationID string) (PassageReassessment, error) {
	if _, ok := r.Runs.Get(runID); !ok {
		return PassageReassessment{}, fmt.Errorf("run %s not found", runID)
	}
	control := r.ensureControl()
	if _, ok := control.Stations[stationID]; !ok {
		return PassageReassessment{}, errors.New("unknown passage station")
	}
	generation := r.reviewGeneration(runID, stationID)
	result := r.recomputeBasis(runID, stationID, generation)

	r.mu.Lock()
	for i := range r.control.Reviews {
		review := &r.control.Reviews[i]
		if review.RunID == runID && review.StationID == stationID && review.Generation == generation && review.Open() {
			result.ReviewOpen = true
		}
	}
	r.mu.Unlock()
	return result, nil
}

// reviewGeneration picks the generation covered by an open review, falling
// back to the tracker's current generation.
func (r *Runtime) reviewGeneration(runID, stationID string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, review := range r.control.Reviews {
		if review.RunID == runID && review.StationID == stationID && review.Open() {
			return review.Generation
		}
	}
	return r.Tracker.Generation(runID)
}

func (r *Runtime) recomputeBasis(runID, stationID string, generation int64) PassageReassessment {
	points := r.Lineage.Evidence(runID, stationID, generation)
	result := PassageReassessment{
		RunID:         runID,
		StationID:     stationID,
		Generation:    generation,
		EvidenceCount: len(points),
	}
	add := func(name string, passed bool, detail string) {
		result.Basis = append(result.Basis, BasisResult{Name: name, Passed: passed, Detail: detail})
	}
	add(model.BasisQuorum, r.Tracker.ArrivalQuorum(points, 2),
		fmt.Sprintf("%d distinct source readings", len(points)))
	add(model.BasisIndependence, tracker.IndependentPhysicalArrival(points, 2),
		"requires two distinct physical probes")
	add(model.BasisContinuity, tracker.Continuous(points, PassageMaxGap),
		fmt.Sprintf("gaps must not exceed %s", PassageMaxGap))
	return result
}

// ResolveReview closes an open review after operator confirmation and clears
// the review flags on the affected permit and tracking states.
func (r *Runtime) ResolveReview(runID, stationID, resolution string) error {
	if strings.TrimSpace(resolution) == "" {
		return errors.New("resolution note is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	found := false
	generations := make(map[int64]bool)
	for i := range r.control.Reviews {
		review := &r.control.Reviews[i]
		if review.RunID == runID && review.StationID == stationID && review.Open() {
			review.ResolvedAt = time.Now().UTC()
			review.Resolution = resolution
			generations[review.Generation] = true
			found = true
		}
	}
	if !found {
		return fmt.Errorf("no open review for run %s at %s", runID, stationID)
	}
	permits := r.control.Permits[runID]
	for i := range permits {
		if generations[permits[i].Generation] {
			permits[i].ReviewRequired = false
		}
	}
	r.control.Permits[runID] = permits
	states := r.control.Tracking[runID]
	for i := range states {
		if states[i].StationID == stationID {
			states[i].ReviewRequired = false
		}
	}
	r.control.Tracking[runID] = states
	r.control.Revision = model.NewRevision()
	return nil
}

// ObservationHistory exposes the full lineage (withdrawn originals included)
// for a station, newest generation first.
func (r *Runtime) ObservationHistory(runID, stationID string) ([]tracker.SourceObservation, error) {
	if _, ok := r.Runs.Get(runID); !ok {
		return nil, fmt.Errorf("run %s not found", runID)
	}
	control := r.ensureControl()
	if _, ok := control.Stations[stationID]; !ok {
		return nil, errors.New("unknown tracking station")
	}
	return r.Lineage.History(runID, stationID), nil
}

// restoreLineage rebuilds the in-memory lineage and coordinator evidence from
// persisted tracking states after recovery.
func (r *Runtime) restoreLineage(states []model.TrackingState) {
	lineage := tracker.NewSourceLineage()
	ordered := append([]model.TrackingState(nil), states...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ObservedAt.Before(ordered[j].ObservedAt) })

	correctionFor := make(map[string]model.TrackingState)
	for _, state := range ordered {
		if state.CorrectsObservationID != "" {
			correctionFor[state.CorrectsObservationID] = state
		}
	}

	// Add originals first (withdrawn records keep their flags), then the
	// corrections that link to them.
	for _, state := range ordered {
		if state.ObservationID == "" || state.CorrectsObservationID != "" {
			continue
		}
		observation := lineageObservation(state)
		if err := lineage.Add(observation); err != nil {
			continue
		}
		if state.Withdrawn {
			correctedBy := ""
			if correction, ok := correctionFor[state.ObservationID]; ok {
				correctedBy = correction.ObservationID
			}
			withdrawnAt := state.ObservedAt
			if correction, ok := correctionFor[state.ObservationID]; ok {
				withdrawnAt = correction.ObservedAt
			}
			_, _ = lineage.Withdraw(state.ObservationID, state.WithdrawReason, correctedBy, withdrawnAt)
		}
	}
	for _, state := range ordered {
		if state.ObservationID == "" || state.CorrectsObservationID == "" {
			continue
		}
		observation := lineageObservation(state)
		observation.WithdrawReason = state.WithdrawReason
		_ = lineage.Add(observation)
	}
	r.Lineage = lineage

	coordinator := tracker.NewCoordinator()
	maxGeneration := make(map[string]int64)
	active := make([]model.TrackingState, 0, len(ordered))
	for _, state := range ordered {
		if state.Withdrawn || state.ObservationID == "" {
			continue
		}
		active = append(active, state)
		if state.Generation > maxGeneration[state.RunID] {
			maxGeneration[state.RunID] = state.Generation
		}
	}
	for runID, generation := range maxGeneration {
		for coordinator.Generation(runID) < generation {
			coordinator.Revise(runID)
		}
	}
	for _, state := range active {
		coordinator.ObserveWithID(state.ObservationID, state.RunID, state.StationID,
			state.SourceID, state.Position, state.ObservedAt)
	}
	r.Tracker = coordinator
}

func lineageObservation(state model.TrackingState) tracker.SourceObservation {
	kind := tracker.SourceKind(state.Kind)
	if kind == "" {
		kind = tracker.SourceMagnetic
	}
	return tracker.SourceObservation{
		RunID:                 state.RunID,
		StationID:             state.StationID,
		ObservationID:         state.ObservationID,
		PhysicalSourceID:      state.SourceID,
		CorrectsObservationID: state.CorrectsObservationID,
		Kind:                  kind,
		Position:              state.Position,
		Generation:            state.Generation,
		ObservedAt:            state.ObservedAt,
		Valid:                 true,
	}
}
