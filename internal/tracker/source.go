package tracker

import (
	"errors"
	"sort"
	"strings"
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

type SourceObservation struct {
	RunID                 string     `json:"run_id"`
	StationID             string     `json:"station_id"`
	ObservationID         string     `json:"observation_id"`
	PhysicalSourceID      string     `json:"physical_source_id"`
	ParentObservationID   string     `json:"parent_observation_id,omitempty"`
	CorrectsObservationID string     `json:"corrects_observation_id,omitempty"`
	Kind                  SourceKind `json:"kind"`
	Position              float64    `json:"position"`
	Generation            int64      `json:"generation"`
	ObservedAt            time.Time  `json:"observed_at"`
	Valid                 bool       `json:"valid"`

	// Withdrawal lineage: the original observation is never deleted. A
	// correction links back through CorrectsObservationID and the withdrawn
	// original points forward through CorrectedBy.
	Withdrawn      bool      `json:"withdrawn,omitempty"`
	WithdrawnAt    time.Time `json:"withdrawn_at,omitempty"`
	WithdrawReason string    `json:"withdraw_reason,omitempty"`
	CorrectedBy    string    `json:"corrected_by,omitempty"`
}

// CorrectionRecord preserves the original observation alongside its
// correction for audit and replay.
type CorrectionRecord struct {
	RunID      string            `json:"run_id"`
	StationID  string            `json:"station_id"`
	Generation int64             `json:"generation"`
	Original   SourceObservation `json:"original"`
	Correction SourceObservation `json:"correction"`
	Reason     string            `json:"reason"`
	At         time.Time         `json:"at"`
}

type SourceLineage struct {
	observations map[string]SourceObservation
	children     map[string][]string
}

func NewSourceLineage() *SourceLineage {
	return &SourceLineage{
		observations: make(map[string]SourceObservation),
		children:     make(map[string][]string),
	}
}

func (l *SourceLineage) Add(observation SourceObservation) error {
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
	if observation.ParentObservationID != "" && observation.CorrectsObservationID != "" {
		return errors.New("observation cannot be both derived and corrective")
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
		l.children[parent.ObservationID] = append(l.children[parent.ObservationID], observation.ObservationID)
	}
	if observation.CorrectsObservationID != "" {
		target, ok := l.observations[observation.CorrectsObservationID]
		if !ok {
			return errors.New("corrected observation does not exist")
		}
		if target.Withdrawn && target.CorrectedBy != observation.ObservationID {
			return errors.New("corrected observation is already withdrawn")
		}
		if target.RunID != observation.RunID || target.StationID != observation.StationID {
			return errors.New("correction crosses run or station")
		}
		if target.Generation != observation.Generation {
			return errors.New("correction changes generation")
		}
		if !target.Withdrawn {
			target.Withdrawn = true
			target.WithdrawnAt = observation.ObservedAt
			target.CorrectedBy = observation.ObservationID
			if reason := strings.TrimSpace(observation.WithdrawReason); reason != "" {
				target.WithdrawReason = reason
			} else {
				target.WithdrawReason = "superseded by correction"
			}
			l.observations[target.ObservationID] = target
		}
	}
	l.observations[observation.ObservationID] = observation
	return nil
}

// Withdraw marks the observation and every observation derived from it as
// withdrawn. It returns every observation that changed so callers can retract
// the matching coordinator points. Withdrawn observations remain in the
// lineage; pass an empty replacementID for a plain retraction.
func (l *SourceLineage) Withdraw(observationID, reason, replacementID string, at time.Time) ([]SourceObservation, error) {
	target, ok := l.observations[observationID]
	if !ok {
		return nil, errors.New("observation does not exist")
	}
	if target.Withdrawn {
		return nil, errors.New("observation is already withdrawn")
	}
	if strings.TrimSpace(reason) == "" {
		return nil, errors.New("withdrawal reason is required")
	}
	if at.IsZero() {
		return nil, errors.New("withdrawal time is required")
	}
	ids := []string{observationID}
	for _, child := range l.children[observationID] {
		ids = append(ids, child)
	}
	changed := make([]SourceObservation, 0, len(ids))
	for _, id := range ids {
		current := l.observations[id]
		current.Withdrawn = true
		current.WithdrawnAt = at
		current.WithdrawReason = reason
		current.CorrectedBy = replacementID
		l.observations[id] = current
		changed = append(changed, current)
	}
	return changed, nil
}

func (l *SourceLineage) Observation(observationID string) (SourceObservation, bool) {
	observation, ok := l.observations[observationID]
	return observation, ok
}

// active reports whether an observation still counts as passage evidence:
// it must be valid, not withdrawn and inherit from an active parent.
func (l *SourceLineage) active(observation SourceObservation) bool {
	if !observation.Valid || observation.Withdrawn {
		return false
	}
	if observation.ParentObservationID != "" {
		parent, ok := l.observations[observation.ParentObservationID]
		if !ok || !l.active(parent) {
			return false
		}
	}
	return true
}

// Observations returns every observation for the run/station/generation,
// including withdrawn records, ordered by observation time.
func (l *SourceLineage) Observations(runID, stationID string, generation int64) []SourceObservation {
	out := make([]SourceObservation, 0)
	for _, observation := range l.observations {
		if observation.RunID == runID && observation.StationID == stationID && observation.Generation == generation {
			out = append(out, observation)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ObservedAt.Equal(out[j].ObservedAt) {
			return out[i].ObservationID < out[j].ObservationID
		}
		return out[i].ObservedAt.Before(out[j].ObservedAt)
	})
	return out
}

// History returns every observation recorded at the station across all
// generations, withdrawn records included, newest generation first.
func (l *SourceLineage) History(runID, stationID string) []SourceObservation {
	out := make([]SourceObservation, 0)
	for _, observation := range l.observations {
		if observation.RunID == runID && observation.StationID == stationID {
			out = append(out, observation)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Generation != out[j].Generation {
			return out[i].Generation > out[j].Generation
		}
		if out[i].ObservedAt.Equal(out[j].ObservedAt) {
			return out[i].ObservationID < out[j].ObservationID
		}
		return out[i].ObservedAt.Before(out[j].ObservedAt)
	})
	return out
}

// Evidence converts the active (non-withdrawn) observations of one generation
// into passage evidence.
func (l *SourceLineage) Evidence(runID, stationID string, generation int64) []model.PassageEvidence {
	observations := l.Observations(runID, stationID, generation)
	evidence := make([]model.PassageEvidence, 0, len(observations))
	for _, observation := range observations {
		if !l.active(observation) {
			continue
		}
		evidence = append(evidence, PassageEvidence(observation))
	}
	return evidence
}

// PassageEvidence maps a lineage observation onto the shared evidence model.
func PassageEvidence(observation SourceObservation) model.PassageEvidence {
	return model.PassageEvidence{
		ObservationID: observation.ObservationID,
		RunID:         observation.RunID,
		Station:       observation.StationID,
		SourceID:      observation.PhysicalSourceID,
		Generation:    observation.Generation,
		Position:      observation.Position,
		At:            observation.ObservedAt,
		Valid:         observation.Valid,
	}
}

// Corrections returns the original/correction pairs recorded for a run,
// ordered by the time the correction was made.
func (l *SourceLineage) Corrections(runID string) []CorrectionRecord {
	out := make([]CorrectionRecord, 0)
	for _, observation := range l.observations {
		if observation.CorrectsObservationID == "" || observation.RunID != runID {
			continue
		}
		original, ok := l.observations[observation.CorrectsObservationID]
		if !ok {
			continue
		}
		out = append(out, CorrectionRecord{
			RunID:      observation.RunID,
			StationID:  observation.StationID,
			Generation: observation.Generation,
			Original:   original,
			Correction: observation,
			Reason:     original.WithdrawReason,
			At:         observation.ObservedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].At.Equal(out[j].At) {
			return out[i].Original.ObservationID < out[j].Original.ObservationID
		}
		return out[i].At.Before(out[j].At)
	})
	return out
}

// IndependentPhysicalArrival reports whether the evidence draws on at least
// the required number of distinct physical probes. A source id may carry a
// channel suffix after "/"; the physical probe is the prefix.
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

// Continuous reports whether the evidence forms an unbroken chain: at least
// two observations, a single shared generation, and no gap longer than
// maxGap between adjacent observations.
func Continuous(events []model.PassageEvidence, maxGap time.Duration) bool {
	if len(events) < 2 {
		return false
	}
	ordered := append([]model.PassageEvidence(nil), events...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].At.Equal(ordered[j].At) {
			return ordered[i].ObservationID < ordered[j].ObservationID
		}
		return ordered[i].At.Before(ordered[j].At)
	})
	for i := 1; i < len(ordered); i++ {
		if ordered[i].At.Sub(ordered[i-1].At) > maxGap {
			return false
		}
		if ordered[i].Generation != ordered[0].Generation {
			return false
		}
	}
	return true
}
