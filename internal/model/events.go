package model

import "time"

type Run struct {
	ID        string
	Station   string
	State     RunState
	Epoch     int64
	CreatedAt time.Time
	UpdatedAt time.Time
}
type Event struct {
	ID      string
	RunID   string
	Kind    string
	Epoch   int64
	Payload string
	At      time.Time
	Durable bool
}
type PassageEvidence struct {
	ObservationID string
	RunID         string
	Station       string
	SourceID      string
	Generation    int64
	Position      float64
	At            time.Time
	Valid         bool
}
type RouteLease struct {
	Route      string
	RunID      string
	Epoch      int64
	AcquiredAt time.Time
}
type CommandReceipt struct {
	RunID    string
	Command  string
	Epoch    int64
	Accepted bool
	At       time.Time
}
type Equipment struct {
	Name      string
	Kind      string
	State     string
	UpdatedAt time.Time
}
type Interlock struct {
	Name   string
	Ready  bool
	Reason string
}
type Incident struct {
	ID       string
	RunID    string
	Severity string
	Message  string
	At       time.Time
}
