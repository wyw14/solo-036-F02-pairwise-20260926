package model

import (
	"errors"
	"strings"
	"time"
)

// ReviewBasis lists the passage basis dimensions that may be recomputed after a
// source withdrawal.
const (
	BasisIndependence = "independence"
	BasisContinuity   = "continuity"
	BasisQuorum       = "quorum"
)

// RunReview marks a run decision (e.g. a granted passage permit) whose
// supporting evidence was withdrawn or corrected and must be rechecked.
type RunReview struct {
	RunID         string    `json:"run_id"`
	StationID     string    `json:"station_id"`
	Generation    int64     `json:"generation"`
	Reason        string    `json:"reason"`
	AffectedBasis []string  `json:"affected_basis"`
	Withdrawn     []string  `json:"withdrawn_observations"`
	OpenedAt      time.Time `json:"opened_at"`
	ResolvedAt    time.Time `json:"resolved_at,omitempty"`
	Resolution    string    `json:"resolution,omitempty"`
}

// Open reports whether the review still requires operator action.
func (v RunReview) Open() bool { return !v.OpenedAt.IsZero() && v.ResolvedAt.IsZero() }

func (v RunReview) Validate() error {
	if v.RunID == "" || v.StationID == "" {
		return errors.New("review identity is incomplete")
	}
	if v.Generation <= 0 {
		return errors.New("review generation must be positive")
	}
	if strings.TrimSpace(v.Reason) == "" {
		return errors.New("review reason is required")
	}
	if v.OpenedAt.IsZero() {
		return errors.New("review open time is required")
	}
	if !v.ResolvedAt.IsZero() && strings.TrimSpace(v.Resolution) == "" {
		return errors.New("resolved review must provide a resolution")
	}
	for _, basis := range v.AffectedBasis {
		switch basis {
		case BasisIndependence, BasisContinuity, BasisQuorum:
		default:
			return errors.New("review references an unknown basis")
		}
	}
	return nil
}
