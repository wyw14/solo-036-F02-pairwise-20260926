package model

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type StationRole string

const (
	StationLauncher     StationRole = "launcher"
	StationIntermediate StationRole = "intermediate"
	StationReceiver     StationRole = "receiver"
)

type ValvePosition string

const (
	ValveClosed  ValvePosition = "closed"
	ValveOpening ValvePosition = "opening"
	ValveOpen    ValvePosition = "open"
	ValveClosing ValvePosition = "closing"
)

type CompressorMode string

const (
	CompressorStandby CompressorMode = "standby"
	CompressorRamping CompressorMode = "ramping"
	CompressorHolding CompressorMode = "holding"
	CompressorStopped CompressorMode = "stopped"
)

type Station struct {
	ID            string      `json:"id"`
	Name          string      `json:"name"`
	Role          StationRole `json:"role"`
	Kilometer     float64     `json:"kilometer"`
	PressureLimit float64     `json:"pressure_limit"`
	Enabled       bool        `json:"enabled"`
}

type ValveState struct {
	ID           string        `json:"id"`
	StationID    string        `json:"station_id"`
	Position     ValvePosition `json:"position"`
	RunID        string        `json:"run_id,omitempty"`
	CommandEpoch int64         `json:"command_epoch"`
	ObservedAt   time.Time     `json:"observed_at"`
}

type ClosureState struct {
	StationID    string    `json:"station_id"`
	RunID        string    `json:"run_id,omitempty"`
	CommandEpoch int64     `json:"command_epoch"`
	Locked       bool      `json:"locked"`
	ObservedAt   time.Time `json:"observed_at"`
}

type PressureState struct {
	StationID  string         `json:"station_id"`
	RunID      string         `json:"run_id,omitempty"`
	Value      float64        `json:"value"`
	Target     float64        `json:"target"`
	Mode       CompressorMode `json:"mode"`
	ObservedAt time.Time      `json:"observed_at"`
}

type RouteState struct {
	ID          string    `json:"id"`
	FromStation string    `json:"from_station"`
	ToStation   string    `json:"to_station"`
	RunID       string    `json:"run_id,omitempty"`
	LeaseEpoch  int64     `json:"lease_epoch"`
	Reserved    bool      `json:"reserved"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type TrackingState struct {
	RunID         string    `json:"run_id"`
	StationID     string    `json:"station_id"`
	SourceID      string    `json:"source_id"`
	ObservationID string    `json:"observation_id,omitempty"`
	Generation    int64     `json:"generation"`
	Position      float64   `json:"position"`
	Online        bool      `json:"online"`
	ObservedAt    time.Time `json:"observed_at"`
}

type PermitState struct {
	RunID          string    `json:"run_id"`
	StationID      string    `json:"station_id,omitempty"`
	Name           string    `json:"name"`
	Generation     int64     `json:"generation"`
	Granted        bool      `json:"granted"`
	Reason         string    `json:"reason"`
	ReviewRequired bool      `json:"review_required,omitempty"`
	ReviewReason   string    `json:"review_reason,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type OperationRecord struct {
	Run       Run             `json:"run"`
	Route     RouteState      `json:"route"`
	Pressure  PressureState   `json:"pressure"`
	Tracking  []TrackingState `json:"tracking"`
	Permits   []PermitState   `json:"permits"`
	LastError string          `json:"last_error,omitempty"`
}

type ControlSnapshot struct {
	Revision   string          `json:"revision"`
	CapturedAt time.Time       `json:"captured_at"`
	Stations   []Station       `json:"stations"`
	Valves     []ValveState    `json:"valves"`
	Closures   []ClosureState  `json:"closures"`
	Pressures  []PressureState `json:"pressures"`
	Routes     []RouteState    `json:"routes"`
	Tracking   []TrackingState `json:"tracking"`
	Permits    []PermitState   `json:"permits"`
	Runs       []Run           `json:"runs"`
	Incidents  []Incident      `json:"incidents"`
}

func (s Station) Validate() error {
	if strings.TrimSpace(s.ID) == "" {
		return errors.New("station id is required")
	}
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("station name is required")
	}
	if s.Role != StationLauncher && s.Role != StationIntermediate && s.Role != StationReceiver {
		return fmt.Errorf("invalid station role %q", s.Role)
	}
	if s.Kilometer < 0 {
		return errors.New("station kilometer must be non-negative")
	}
	if s.PressureLimit <= 0 {
		return errors.New("station pressure limit must be positive")
	}
	return nil
}

func (v ValveState) Validate() error {
	if v.ID == "" || v.StationID == "" {
		return errors.New("valve identity is incomplete")
	}
	if v.CommandEpoch < 0 {
		return errors.New("valve command epoch must be non-negative")
	}
	switch v.Position {
	case ValveClosed, ValveOpening, ValveOpen, ValveClosing:
		return nil
	default:
		return fmt.Errorf("invalid valve position %q", v.Position)
	}
}

func (p PressureState) Validate(limit float64) error {
	if p.StationID == "" {
		return errors.New("pressure station is required")
	}
	if p.Value < 0 || p.Target < 0 {
		return errors.New("pressure values must be non-negative")
	}
	if limit > 0 && (p.Value > limit || p.Target > limit) {
		return fmt.Errorf("pressure exceeds station limit %.2f", limit)
	}
	switch p.Mode {
	case CompressorStandby, CompressorRamping, CompressorHolding, CompressorStopped:
		return nil
	default:
		return fmt.Errorf("invalid compressor mode %q", p.Mode)
	}
}

func (r RouteState) Validate(stations map[string]Station) error {
	if r.ID == "" {
		return errors.New("route id is required")
	}
	from, fromOK := stations[r.FromStation]
	to, toOK := stations[r.ToStation]
	if !fromOK || !toOK {
		return errors.New("route references an unknown station")
	}
	if from.Kilometer >= to.Kilometer {
		return errors.New("route stations are not in pipeline order")
	}
	if r.Reserved && (r.RunID == "" || r.LeaseEpoch <= 0) {
		return errors.New("reserved route has no fenced owner")
	}
	return nil
}

func (t TrackingState) Validate(stations map[string]Station) error {
	if t.RunID == "" || t.SourceID == "" {
		return errors.New("tracking identity is incomplete")
	}
	station, ok := stations[t.StationID]
	if !ok {
		return errors.New("tracking references an unknown station")
	}
	if t.Generation <= 0 {
		return errors.New("tracking generation must be positive")
	}
	if t.Position < 0 || t.Position > station.Kilometer+1000 {
		return errors.New("tracking position is outside the station window")
	}
	return nil
}

func (p PermitState) Validate() error {
	if p.RunID == "" || p.Name == "" {
		return errors.New("permit identity is incomplete")
	}
	if p.Generation <= 0 {
		return errors.New("permit generation must be positive")
	}
	if !p.Granted && strings.TrimSpace(p.Reason) == "" {
		return errors.New("closed permit must provide a reason")
	}
	return nil
}

func (s ControlSnapshot) StationIndex() map[string]Station {
	out := make(map[string]Station, len(s.Stations))
	for _, station := range s.Stations {
		out[station.ID] = station
	}
	return out
}

func (s ControlSnapshot) Validate() error {
	stations := s.StationIndex()
	if len(stations) < 2 {
		return errors.New("at least two stations are required")
	}
	for _, station := range s.Stations {
		if err := station.Validate(); err != nil {
			return fmt.Errorf("station %s: %w", station.ID, err)
		}
	}
	for _, valve := range s.Valves {
		if err := valve.Validate(); err != nil {
			return fmt.Errorf("valve %s: %w", valve.ID, err)
		}
	}
	for _, pressure := range s.Pressures {
		station, ok := stations[pressure.StationID]
		if !ok {
			return fmt.Errorf("pressure references unknown station %s", pressure.StationID)
		}
		if err := pressure.Validate(station.PressureLimit); err != nil {
			return fmt.Errorf("pressure %s: %w", pressure.StationID, err)
		}
	}
	for _, route := range s.Routes {
		if err := route.Validate(stations); err != nil {
			return fmt.Errorf("route %s: %w", route.ID, err)
		}
	}
	for _, tracking := range s.Tracking {
		if err := tracking.Validate(stations); err != nil {
			return fmt.Errorf("tracking %s: %w", tracking.RunID, err)
		}
	}
	for _, permit := range s.Permits {
		if err := permit.Validate(); err != nil {
			return fmt.Errorf("permit %s: %w", permit.Name, err)
		}
	}
	return nil
}

func (s ControlSnapshot) SortedStations() []Station {
	out := append([]Station(nil), s.Stations...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kilometer == out[j].Kilometer {
			return out[i].ID < out[j].ID
		}
		return out[i].Kilometer < out[j].Kilometer
	})
	return out
}

func (s ControlSnapshot) Operation(runID string) (OperationRecord, bool) {
	var result OperationRecord
	found := false
	for _, run := range s.Runs {
		if run.ID == runID {
			result.Run = run
			found = true
			break
		}
	}
	if !found {
		return OperationRecord{}, false
	}
	for _, route := range s.Routes {
		if route.RunID == runID {
			result.Route = route
			break
		}
	}
	for _, pressure := range s.Pressures {
		if pressure.RunID == runID {
			result.Pressure = pressure
			break
		}
	}
	for _, tracking := range s.Tracking {
		if tracking.RunID == runID {
			result.Tracking = append(result.Tracking, tracking)
		}
	}
	for _, permit := range s.Permits {
		if permit.RunID == runID {
			result.Permits = append(result.Permits, permit)
		}
	}
	return result, true
}
