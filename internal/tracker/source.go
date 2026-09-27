package tracker

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wyw14/solo-036/internal/model"
)

type SourceKind string

const (
	SourcePressure        SourceKind = "pressure"
	SourceAcoustic        SourceKind = "acoustic"
	SourceMagnetic        SourceKind = "magnetic"
	SourceDerivedPosition SourceKind = "derived_position"
)

type ObservationStatus string

const (
	ObservationActive    ObservationStatus = "active"
	ObservationWithdrawn ObservationStatus = "withdrawn"
)

type SourceObservation struct {
	RunID                 string            `json:"run_id"`
	StationID             string            `json:"station_id"`
	ObservationID         string            `json:"observation_id"`
	PhysicalSourceID      string            `json:"physical_source_id"`
	ParentObservationID   string            `json:"parent_observation_id,omitempty"`
	CorrectsObservationID string            `json:"corrects_observation_id,omitempty"`
	Kind                  SourceKind        `json:"kind"`
	Position              float64           `json:"position"`
	Generation            int64             `json:"generation"`
	ObservedAt            time.Time         `json:"observed_at"`
	Valid                 bool              `json:"valid"`
	Status                ObservationStatus `json:"status"`
	WithdrawnAt           time.Time         `json:"withdrawn_at,omitempty"`
	WithdrawReason        string            `json:"withdraw_reason,omitempty"`
}

// Correction preserves the relationship between an original observation and
// its withdrawal or correction. The original observation is never deleted.
type Correction struct {
	CorrectionID  string    `json:"correction_id"`
	RunID         string    `json:"run_id"`
	StationID     string    `json:"station_id"`
	PrimaryID     string    `json:"primary_id"`
	CascadedIDs   []string  `json:"cascaded_ids,omitempty"`
	ReplacementID string    `json:"replacement_id,omitempty"`
	Reason        string    `json:"reason"`
	At            time.Time `json:"at"`
}

// PassageBasis is the recomputed passage basis for a run/station/generation
// after the active observation set changed.
type PassageBasis struct {
	RunID                      string                  `json:"run_id"`
	StationID                  string                  `json:"station_id"`
	Generation                 int64                   `json:"generation"`
	Evidence                   []model.PassageEvidence `json:"evidence"`
	IndependentPhysicalSources []string                `json:"independent_physical_sources"`
	RequiredIndependent        int                     `json:"required_independent"`
	Independent                bool                    `json:"independent"`
	RequiredQuorum             int                     `json:"required_quorum"`
	Quorum                     bool                    `json:"quorum"`
	Continuous                 bool                    `json:"continuous"`
	MaxGap                     time.Duration           `json:"max_gap"`
}

// Satisfied reports whether the basis still supports a passage decision.
func (b PassageBasis) Satisfied() bool {
	return b.Quorum && b.Independent && b.Continuous
}

// PassageReview records a passage decision together with the exact evidence
// basis it was granted on, so later withdrawals can flag it for re-check.
type PassageReview struct {
	RunID              string       `json:"run_id"`
	StationID          string       `json:"station_id"`
	Generation         int64        `json:"generation"`
	GrantedAt          time.Time    `json:"granted_at"`
	ProofObservationID string       `json:"proof_observation_id"`
	Basis              PassageBasis `json:"basis"`
	ReviewRequired     bool         `json:"review_required"`
	ReviewReason       string       `json:"review_reason,omitempty"`
}

type SourceLineage struct {
	mu           sync.Mutex
	observations map[string]SourceObservation
	children     map[string][]string
	correcting   map[string][]string
	corrections  []Correction
	reviews      map[string][]PassageReview
}

func NewSourceLineage() *SourceLineage {
	return &SourceLineage{
		observations: make(map[string]SourceObservation),
		children:     make(map[string][]string),
		correcting:   make(map[string][]string),
		corrections:  make([]Correction, 0),
		reviews:      make(map[string][]PassageReview),
	}
}

func reviewKey(runID, stationID string) string { return runID + "/" + stationID }

// Add records a new observation. When the observation corrects an earlier
// one, the returned Correction describes the observations withdrawn as a
// result (nil otherwise).
func (l *SourceLineage) Add(observation SourceObservation) (*Correction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.validateObservation(observation); err != nil {
		return nil, err
	}
	if observation.Status == "" {
		observation.Status = ObservationActive
	}
	if observation.Status != ObservationActive {
		return nil, errors.New("new observations must be active")
	}
	if observation.ParentObservationID != "" {
		parent := l.observations[observation.ParentObservationID]
		if parent.Status == ObservationWithdrawn {
			return nil, errors.New("parent observation has been withdrawn")
		}
		l.children[parent.ObservationID] = append(l.children[parent.ObservationID], observation.ObservationID)
	}
	l.observations[observation.ObservationID] = observation
	if observation.CorrectsObservationID == "" {
		return nil, nil
	}
	target := l.observations[observation.CorrectsObservationID]
	now := time.Now().UTC()
	// validateObservation guarantees the target exists and is active, so the
	// withdrawal cannot fail.
	cascaded, _ := l.withdrawLocked(target, observation.ObservationID, "corrected by "+observation.ObservationID, now)
	l.correcting[target.ObservationID] = append(l.correcting[target.ObservationID], observation.ObservationID)
	correction := Correction{
		CorrectionID:  model.NewRevision(),
		RunID:         target.RunID,
		StationID:     target.StationID,
		PrimaryID:     target.ObservationID,
		CascadedIDs:   cascaded,
		ReplacementID: observation.ObservationID,
		Reason:        "corrected observation",
		At:            now,
	}
	l.corrections = append(l.corrections, correction)
	return &correction, nil
}

func (l *SourceLineage) validateObservation(observation SourceObservation) error {
	if strings.TrimSpace(observation.ObservationID) == "" {
		return errors.New("observation id is required")
	}
	if strings.TrimSpace(observation.PhysicalSourceID) == "" {
		return errors.New("physical source id is required")
	}
	if observation.RunID == "" || observation.StationID == "" {
		return errors.New("observation run and station are required")
	}
	if observation.Generation <= 0 {
		return errors.New("observation generation must be positive")
	}
	if observation.ObservedAt.IsZero() {
		return errors.New("observation time is required")
	}
	switch observation.Kind {
	case SourcePressure, SourceAcoustic, SourceMagnetic, SourceDerivedPosition:
	default:
		return errors.New("observation source kind is invalid")
	}
	if _, exists := l.observations[observation.ObservationID]; exists {
		return errors.New("observation id already exists")
	}
	if observation.ParentObservationID != "" {
		parent, ok := l.observations[observation.ParentObservationID]
		if !ok {
			return errors.New("parent observation does not exist")
		}
		if parent.RunID != observation.RunID || parent.StationID != observation.StationID {
			return errors.New("derived observation crosses run or station")
		}
		if parent.PhysicalSourceID != observation.PhysicalSourceID {
			return errors.New("derived observation changes physical source")
		}
		if parent.Generation != observation.Generation {
			return errors.New("derived observation changes generation")
		}
	}
	if observation.CorrectsObservationID != "" {
		if observation.CorrectsObservationID == observation.ObservationID {
			return errors.New("observation cannot correct itself")
		}
		target, ok := l.observations[observation.CorrectsObservationID]
		if !ok {
			return errors.New("corrected observation does not exist")
		}
		if target.RunID != observation.RunID || target.StationID != observation.StationID {
			return errors.New("correction crosses run or station")
		}
		if target.Status == ObservationWithdrawn {
			return errors.New("corrected observation has already been withdrawn")
		}
		if target.Generation != observation.Generation {
			return errors.New("correction changes generation")
		}
	}
	return nil
}

// Withdraw retracts an active observation (and any observations derived from
// it) without deleting anything and without bumping the generation. The
// returned list includes the withdrawn observation id followed by cascaded
// descendant ids.
func (l *SourceLineage) Withdraw(observationID, reason string, at time.Time) (SourceObservation, []string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	target, ok := l.observations[observationID]
	if !ok {
		return SourceObservation{}, nil, errors.New("observation does not exist")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	cascaded, err := l.withdrawLocked(target, "", reason, at)
	if err != nil {
		return SourceObservation{}, nil, err
	}
	l.corrections = append(l.corrections, Correction{
		CorrectionID: model.NewRevision(),
		RunID:        target.RunID,
		StationID:    target.StationID,
		PrimaryID:    target.ObservationID,
		CascadedIDs:  cascaded,
		Reason:       reason,
		At:           at,
	})
	return l.observations[observationID], append([]string{observationID}, cascaded...), nil
}

// withdrawLocked marks the target and its derived subtree as withdrawn and
// returns the cascaded descendant ids (excluding the target itself).
func (l *SourceLineage) withdrawLocked(target SourceObservation, replacementID, reason string, at time.Time) ([]string, error) {
	if target.Status == ObservationWithdrawn {
		return nil, errors.New("observation has already been withdrawn")
	}
	cascaded := make([]string, 0)
	var walk func(id string)
	walk = func(id string) {
		for _, childID := range l.children[id] {
			child := l.observations[childID]
			if child.Status == ObservationWithdrawn {
				continue
			}
			child.Status = ObservationWithdrawn
			child.WithdrawnAt = at
			child.WithdrawReason = reason
			l.observations[childID] = child
			cascaded = append(cascaded, childID)
			walk(childID)
		}
	}
	walk(target.ObservationID)
	target.Status = ObservationWithdrawn
	target.WithdrawnAt = at
	target.WithdrawReason = reason
	l.observations[target.ObservationID] = target
	return cascaded, nil
}

func (l *SourceLineage) Get(observationID string) (SourceObservation, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	observation, ok := l.observations[observationID]
	return observation, ok
}

// Observations returns the active observations for a run/station/generation.
// Withdrawn observations are excluded so they no longer feed passage basis.
func (l *SourceLineage) Observations(runID, stationID string, generation int64) []SourceObservation {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.observationsLocked(runID, stationID, generation, false)
}

// History returns every observation recorded for a run/station, including
// withdrawn observations and their correction links, in observation order.
func (l *SourceLineage) History(runID, stationID string) []SourceObservation {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]SourceObservation, 0)
	for _, observation := range l.observations {
		if observation.RunID == runID && observation.StationID == stationID {
			out = append(out, observation)
		}
	}
	sortObservations(out)
	return out
}

// RunHistory returns every observation recorded for a run across all
// stations, including withdrawn observations, in observation order.
func (l *SourceLineage) RunHistory(runID string) []SourceObservation {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]SourceObservation, 0)
	for _, observation := range l.observations {
		if observation.RunID == runID {
			out = append(out, observation)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ObservedAt.Equal(out[j].ObservedAt) {
			if out[i].StationID == out[j].StationID {
				return out[i].ObservationID < out[j].ObservationID
			}
			return out[i].StationID < out[j].StationID
		}
		return out[i].ObservedAt.Before(out[j].ObservedAt)
	})
	return out
}

// Corrections returns the full withdrawal/correction ledger, oldest first.
func (l *SourceLineage) Corrections() []Correction {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Correction(nil), l.corrections...)
}

// CorrectionsForRun returns the withdrawal/correction ledger for one run,
// oldest first.
func (l *SourceLineage) CorrectionsForRun(runID string) []Correction {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Correction, 0)
	for _, correction := range l.corrections {
		if correction.RunID == runID {
			out = append(out, correction)
		}
	}
	return out
}

func (l *SourceLineage) observationsLocked(runID, stationID string, generation int64, includeWithdrawn bool) []SourceObservation {
	out := make([]SourceObservation, 0)
	for _, observation := range l.observations {
		if observation.RunID != runID || observation.StationID != stationID || observation.Generation != generation {
			continue
		}
		if !includeWithdrawn && observation.Status == ObservationWithdrawn {
			continue
		}
		out = append(out, observation)
	}
	sortObservations(out)
	return out
}

func sortObservations(out []SourceObservation) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].ObservedAt.Equal(out[j].ObservedAt) {
			return out[i].ObservationID < out[j].ObservationID
		}
		return out[i].ObservedAt.Before(out[j].ObservedAt)
	})
}

func (l *SourceLineage) Evidence(runID, stationID string, generation int64) []model.PassageEvidence {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.evidenceLocked(runID, stationID, generation)
}

func (l *SourceLineage) evidenceLocked(runID, stationID string, generation int64) []model.PassageEvidence {
	observations := l.observationsLocked(runID, stationID, generation, false)
	evidence := make([]model.PassageEvidence, 0, len(observations))
	for _, observation := range observations {
		evidence = append(evidence, model.PassageEvidence{
			RunID:         observation.RunID,
			Station:       observation.StationID,
			SourceID:      observation.PhysicalSourceID,
			ObservationID: observation.ObservationID,
			Generation:    observation.Generation,
			Position:      observation.Position,
			At:            observation.ObservedAt,
			Valid:         observation.Valid,
		})
	}
	return evidence
}

// Basis recomputes the passage basis (source quorum, physical-source
// independence and continuity) from the currently active observations.
func (l *SourceLineage) Basis(runID, stationID string, generation int64, required int, maxGap time.Duration) PassageBasis {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.basisLocked(runID, stationID, generation, required, maxGap)
}

func (l *SourceLineage) basisLocked(runID, stationID string, generation int64, required int, maxGap time.Duration) PassageBasis {
	evidence := l.evidenceLocked(runID, stationID, generation)
	physical := make(map[string]bool)
	sources := make(map[string]bool)
	for _, event := range evidence {
		if !model.ValidEvidence(event) {
			continue
		}
		sources[event.SourceID] = true
		physicalSource := strings.TrimSpace(strings.SplitN(event.SourceID, "/", 2)[0])
		if physicalSource != "" {
			physical[physicalSource] = true
		}
	}
	physicalSources := make([]string, 0, len(physical))
	for source := range physical {
		physicalSources = append(physicalSources, source)
	}
	sort.Strings(physicalSources)
	basis := PassageBasis{
		RunID:                      runID,
		StationID:                  stationID,
		Generation:                 generation,
		Evidence:                   evidence,
		IndependentPhysicalSources: physicalSources,
		RequiredIndependent:        required,
		Independent:                len(physical) >= required,
		RequiredQuorum:             required,
		Quorum:                     len(sources) >= required,
		MaxGap:                     maxGap,
	}
	basis.Continuous = Continuous(evidence, maxGap)
	return basis
}

// RecordPassageGrant snapshots the basis a passage was granted on so a later
// withdrawal can tell whether the decision is affected.
func (l *SourceLineage) RecordPassageGrant(runID, stationID string, generation int64, proofObservationID string, required int, maxGap time.Duration, at time.Time) PassageReview {
	l.mu.Lock()
	defer l.mu.Unlock()
	review := PassageReview{
		RunID:              runID,
		StationID:          stationID,
		Generation:         generation,
		GrantedAt:          at,
		ProofObservationID: proofObservationID,
		Basis:              l.basisLocked(runID, stationID, generation, required, maxGap),
	}
	key := reviewKey(runID, stationID)
	l.reviews[key] = append(l.reviews[key], review)
	return review
}

// MarkAffectedReviews flags every granted passage whose recorded basis relied
// on one of the withdrawn observations (or whose generation/station matches
// when older records carry no observation ids), recomputes its basis and
// returns the affected reviews.
func (l *SourceLineage) MarkAffectedReviews(runID string, withdrawnIDs []string, reason string, required int, maxGap time.Duration) []PassageReview {
	l.mu.Lock()
	defer l.mu.Unlock()
	withdrawn := make(map[string]bool, len(withdrawnIDs))
	for _, id := range withdrawnIDs {
		withdrawn[id] = true
	}
	affected := make([]PassageReview, 0)
	for key, reviews := range l.reviews {
		for i := range reviews {
			review := &reviews[i]
			if review.RunID != runID || review.ReviewRequired {
				continue
			}
			hit := false
			for _, event := range review.Basis.Evidence {
				if event.ObservationID != "" && withdrawn[event.ObservationID] {
					hit = true
					break
				}
			}
			if !hit && len(review.Basis.Evidence) > 0 {
				// All recorded evidence belongs to the same station/generation;
				// a withdrawal in that scope is potentially affecting.
				scope := review.Basis.Evidence[0]
				for id := range withdrawn {
					if observation, ok := l.observations[id]; ok &&
						observation.StationID == scope.Station &&
						observation.Generation == scope.Generation {
						hit = true
						break
					}
				}
			}
			if !hit {
				continue
			}
			review.ReviewRequired = true
			review.ReviewReason = reason
			review.Basis = l.basisLocked(review.RunID, review.StationID, review.Generation, required, maxGap)
			affected = append(affected, *review)
		}
		l.reviews[key] = reviews
	}
	sort.Slice(affected, func(i, j int) bool {
		if affected[i].GrantedAt.Equal(affected[j].GrantedAt) {
			return affected[i].StationID < affected[j].StationID
		}
		return affected[i].GrantedAt.Before(affected[j].GrantedAt)
	})
	return affected
}

// Reviews returns recorded passage decisions for a run, oldest first.
func (l *SourceLineage) Reviews(runID string) []PassageReview {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]PassageReview, 0)
	for key, reviews := range l.reviews {
		if runID == "" || strings.HasPrefix(key, runID+"/") {
			out = append(out, reviews...)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GrantedAt.Equal(out[j].GrantedAt) {
			return out[i].StationID < out[j].StationID
		}
		return out[i].GrantedAt.Before(out[j].GrantedAt)
	})
	return out
}

// ReviewRequired reports whether any passage decision for the run is flagged.
func (l *SourceLineage) ReviewRequired(runID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, reviews := range l.reviews {
		for _, review := range reviews {
			if review.RunID == runID && review.ReviewRequired {
				return true
			}
		}
	}
	return false
}

func IndependentPhysicalArrival(events []model.PassageEvidence, required int) bool {
	if required <= 0 {
		return false
	}
	sources := make(map[string]bool)
	for _, event := range events {
		if !model.ValidEvidence(event) {
			continue
		}
		physicalSource := strings.TrimSpace(strings.SplitN(event.SourceID, "/", 2)[0])
		if physicalSource != "" {
			sources[physicalSource] = true
		}
	}
	return len(sources) >= required
}
