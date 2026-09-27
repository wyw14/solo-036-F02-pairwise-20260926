package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wyw14/solo-036/internal/emergency"
	"github.com/wyw14/solo-036/internal/journal"
	"github.com/wyw14/solo-036/internal/launcher"
	"github.com/wyw14/solo-036/internal/model"
	"github.com/wyw14/solo-036/internal/permit"
	"github.com/wyw14/solo-036/internal/receiver"
	"github.com/wyw14/solo-036/internal/route"
	"github.com/wyw14/solo-036/internal/run"
	"github.com/wyw14/solo-036/internal/tracker"
)

type ControlState struct {
	Network    *route.Network
	Stations   map[string]model.Station
	Routes     map[string]model.RouteState
	Valves     map[string]model.ValveState
	Closures   map[string]model.ClosureState
	Pressures  map[string]model.PressureState
	Tracking   map[string][]model.TrackingState
	Permits    map[string][]model.PermitState
	Incidents  []model.Incident
	Revision   string
	Persistent *journal.FileStore
}

type LaunchRequest struct {
	StationID      string  `json:"station_id"`
	RouteID        string  `json:"route_id"`
	TargetPressure float64 `json:"target_pressure"`
}

type TrackingRequest struct {
	StationID             string             `json:"station_id"`
	SourceID              string             `json:"source_id"`
	Kind                  tracker.SourceKind `json:"kind"`
	Position              float64            `json:"position"`
	ObservedAt            time.Time          `json:"observed_at"`
	CorrectsObservationID string             `json:"corrects_observation_id,omitempty"`
}

type ReceiveRequest struct {
	StationID      string `json:"station_id"`
	IsolationEpoch int64  `json:"isolation_epoch"`
}

type WithdrawObservationRequest struct {
	ObservationID string `json:"observation_id"`
	Reason        string `json:"reason"`
}

// LineageReport exposes the full observation lineage: the original
// observations (including withdrawn ones), the correction ledger, the
// recomputed passage basis and the review flags on affected run decisions.
type LineageReport struct {
	RunID          string                      `json:"run_id"`
	Observations   []tracker.SourceObservation `json:"observations"`
	Corrections    []tracker.Correction        `json:"corrections"`
	Bases          []tracker.PassageBasis      `json:"bases"`
	Reviews        []tracker.PassageReview     `json:"reviews"`
	ReviewRequired bool                        `json:"review_required"`
}

const (
	passageRequired = 2
	passageGap      = 90 * time.Second
)

type RecoveryReport struct {
	RecoveredRuns    int  `json:"recovered_runs"`
	RecoveredRoutes  int  `json:"recovered_routes"`
	RecoveredPermits int  `json:"recovered_permits"`
	JournalEvents    int  `json:"journal_events"`
	Ready            bool `json:"ready"`
}

func defaultControlState() *ControlState {
	network := route.DefaultNetwork()
	stations := network.Stations()
	state := &ControlState{
		Network:   network,
		Stations:  make(map[string]model.Station),
		Routes:    make(map[string]model.RouteState),
		Valves:    make(map[string]model.ValveState),
		Closures:  make(map[string]model.ClosureState),
		Pressures: make(map[string]model.PressureState),
		Tracking:  make(map[string][]model.TrackingState),
		Permits:   make(map[string][]model.PermitState),
		Incidents: make([]model.Incident, 0),
		Revision:  model.NewRevision(),
	}
	for _, station := range stations {
		state.Stations[station.ID] = station
		state.Pressures[station.ID] = model.PressureState{
			StationID:  station.ID,
			Value:      0,
			Target:     0,
			Mode:       model.CompressorStandby,
			ObservedAt: time.Now().UTC(),
		}
	}
	for _, routeState := range network.RouteStates() {
		state.Routes[routeState.ID] = routeState
	}
	for _, stationID := range []string{"station-a", "station-b", "station-c"} {
		state.Valves[stationID+"/main"] = model.ValveState{
			ID:         stationID + "/main",
			StationID:  stationID,
			Position:   model.ValveClosed,
			ObservedAt: time.Now().UTC(),
		}
	}
	return state
}

func (r *Runtime) ensureControl() *ControlState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.control == nil {
		r.control = defaultControlState()
	}
	return r.control
}

func (r *Runtime) ConfigurePersistence(directory string) error {
	store, err := journal.NewFileStore(directory)
	if err != nil {
		return err
	}
	if err := store.Verify(); err != nil {
		return fmt.Errorf("verify persistence: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.control == nil {
		r.control = defaultControlState()
	}
	r.control.Persistent = store
	return nil
}

func (r *Runtime) appendEvent(event model.Event) error {
	if event.ID == "" {
		event.ID = model.NewRevision()
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	if err := r.Journal.Append(event); err != nil {
		return err
	}
	control := r.ensureControl()
	if control.Persistent != nil {
		if err := control.Persistent.Append(event); err != nil {
			return fmt.Errorf("persist event: %w", err)
		}
	}
	return nil
}

func (r *Runtime) recordIncident(runID, severity, message string) model.Incident {
	incident := model.Incident{
		ID:       model.NewRevision(),
		RunID:    runID,
		Severity: severity,
		Message:  message,
		At:       time.Now().UTC(),
	}
	r.mu.Lock()
	if r.control == nil {
		r.control = defaultControlState()
	}
	r.control.Incidents = append(r.control.Incidents, incident)
	r.control.Revision = model.NewRevision()
	r.mu.Unlock()
	_ = r.appendEvent(model.Event{
		RunID:   runID,
		Kind:    "incident",
		Payload: severity + ":" + message,
	})
	return incident
}

func (r *Runtime) ValidateLaunchRequest(request LaunchRequest) error {
	control := r.ensureControl()
	station, ok := control.Stations[request.StationID]
	if !ok {
		return fmt.Errorf("unknown launch station %s", request.StationID)
	}
	if station.Role != model.StationLauncher || !station.Enabled {
		return errors.New("station is not an enabled launcher")
	}
	routeState, ok := control.Routes[request.RouteID]
	if !ok {
		return fmt.Errorf("unknown route %s", request.RouteID)
	}
	if routeState.FromStation != station.ID {
		return errors.New("route does not start at launcher")
	}
	if _, err := control.Network.Path(routeState.FromStation, routeState.ToStation, request.TargetPressure); err != nil {
		return err
	}
	if request.TargetPressure <= 0 || request.TargetPressure > station.PressureLimit {
		return fmt.Errorf("target pressure must be within 0 and %.2f", station.PressureLimit)
	}
	return nil
}

func (r *Runtime) Launch(ctx context.Context, request LaunchRequest) (model.OperationRecord, error) {
	if err := r.ValidateLaunchRequest(request); err != nil {
		return model.OperationRecord{}, err
	}
	created, err := r.CreateOperation(request.StationID)
	if err != nil {
		return model.OperationRecord{}, err
	}
	lease, routeErr := r.Routes.Reserve(request.RouteID, created.ID, created.Epoch)
	if routeErr != nil {
		r.recordIncident(created.ID, "warning", routeErr.Error())
		return model.OperationRecord{}, routeErr
	}
	closureEpoch := created.Epoch + 1
	r.Launcher.BeginClosure(created.ID, closureEpoch)
	closureProof := model.CommandReceipt{
		RunID:    created.ID,
		Command:  "closure",
		Epoch:    closureEpoch,
		Accepted: true,
		At:       time.Now().UTC(),
	}
	valveErr := r.Launcher.ConfirmClosure(closureProof)
	if err := launcher.CombineLaunchResults(valveErr, routeErr); err != nil {
		r.Routes.Release(request.RouteID, created.ID)
		return model.OperationRecord{}, err
	}
	if !r.Launcher.CanLaunch(closureProof) {
		r.Routes.Release(request.RouteID, created.ID)
		return model.OperationRecord{}, errors.New("launcher interlocks rejected current closure proof")
	}
	if err := r.Pressure.Apply(created.ID, request.TargetPressure); err != nil {
		r.Routes.Release(request.RouteID, created.ID)
		return model.OperationRecord{}, err
	}
	if err := run.Start(r.Runs, created.ID); err != nil {
		r.Pressure.Stop()
		r.Routes.Release(request.RouteID, created.ID)
		return model.OperationRecord{}, err
	}
	now := time.Now().UTC()
	r.mu.Lock()
	control := r.control
	routeState := control.Routes[request.RouteID]
	routeState.RunID = created.ID
	routeState.LeaseEpoch = lease.Epoch
	routeState.Reserved = true
	routeState.UpdatedAt = now
	control.Routes[request.RouteID] = routeState
	control.Closures[request.StationID] = model.ClosureState{
		StationID:    request.StationID,
		RunID:        created.ID,
		CommandEpoch: closureEpoch,
		Locked:       true,
		ObservedAt:   now,
	}
	pressureState := control.Pressures[request.StationID]
	pressureState.RunID = created.ID
	pressureState.Value = request.TargetPressure
	pressureState.Target = request.TargetPressure
	pressureState.Mode = model.CompressorHolding
	pressureState.ObservedAt = now
	control.Pressures[request.StationID] = pressureState
	valveState := control.Valves[request.StationID+"/main"]
	valveState.RunID = created.ID
	valveState.Position = model.ValveOpen
	valveState.CommandEpoch = closureEpoch
	valveState.ObservedAt = now
	control.Valves[valveState.ID] = valveState
	control.Revision = model.NewRevision()
	r.mu.Unlock()
	if err := r.appendEvent(model.Event{
		RunID:   created.ID,
		Kind:    "launch",
		Epoch:   closureEpoch,
		Payload: request.RouteID,
		At:      now,
	}); err != nil {
		return model.OperationRecord{}, err
	}
	select {
	case <-ctx.Done():
		_ = r.Cancel(created.ID)
		return model.OperationRecord{}, ctx.Err()
	default:
	}
	return r.Operation(created.ID)
}

func (r *Runtime) Track(runID string, request TrackingRequest) (model.OperationRecord, error) {
	if strings.TrimSpace(request.SourceID) == "" {
		return model.OperationRecord{}, errors.New("tracking source is required")
	}
	control := r.ensureControl()
	station, ok := control.Stations[request.StationID]
	if !ok || !station.Enabled {
		return model.OperationRecord{}, errors.New("tracking station is unavailable")
	}
	current, ok := r.Runs.Get(runID)
	if !ok {
		return model.OperationRecord{}, fmt.Errorf("run %s not found", runID)
	}
	if current.State == model.StateLaunched {
		if err := run.Track(r.Runs, runID); err != nil {
			return model.OperationRecord{}, err
		}
	}
	if request.ObservedAt.IsZero() {
		request.ObservedAt = time.Now().UTC()
	}
	if request.Kind == "" {
		request.Kind = tracker.SourceMagnetic
	}
	observationID := model.NewRevision()
	evidence := r.Tracker.Observe(runID, request.StationID, request.SourceID, observationID, request.Position, request.ObservedAt)
	correction, err := r.Lineage.Add(tracker.SourceObservation{
		RunID:                 runID,
		StationID:             request.StationID,
		ObservationID:         observationID,
		PhysicalSourceID:      request.SourceID,
		CorrectsObservationID: request.CorrectsObservationID,
		Kind:                  request.Kind,
		Position:              request.Position,
		Generation:            evidence.Generation,
		ObservedAt:            request.ObservedAt,
		Valid:                 true,
	})
	if err != nil {
		r.Tracker.Retract(observationID)
		return model.OperationRecord{}, err
	}
	// A correction withdraws the mistyped observation (and anything derived
	// from it); drop the same live points so continuity is recomputed without
	// bumping the generation.
	if correction != nil {
		r.Tracker.Retract(correction.PrimaryID)
		for _, cascadedID := range correction.CascadedIDs {
			r.Tracker.Retract(cascadedID)
		}
		reviewReason := fmt.Sprintf("observation %s corrected by %s", correction.PrimaryID, observationID)
		affected := r.Lineage.MarkAffectedReviews(runID,
			append([]string{correction.PrimaryID}, correction.CascadedIDs...),
			reviewReason, passageRequired, passageGap)
		r.markPermitsForReview(runID, affected, reviewReason)
	}
	if err := r.Routes.Advance(runID, request.Position); err != nil {
		r.recordIncident(runID, "warning", err.Error())
		return model.OperationRecord{}, err
	}
	state := model.TrackingState{
		RunID:         runID,
		StationID:     request.StationID,
		SourceID:      request.SourceID,
		ObservationID: observationID,
		Generation:    evidence.Generation,
		Position:      request.Position,
		Online:        true,
		ObservedAt:    request.ObservedAt,
	}
	r.mu.Lock()
	r.control.Tracking[runID] = append(r.control.Tracking[runID], state)
	r.control.Revision = model.NewRevision()
	r.mu.Unlock()
	if err := r.appendEvent(model.Event{
		RunID:   runID,
		Kind:    "tracking",
		Epoch:   evidence.Generation,
		Payload: fmt.Sprintf("%s/%s/%s/%.3f", request.StationID, observationID, request.SourceID, request.Position),
		At:      request.ObservedAt,
	}); err != nil {
		return model.OperationRecord{}, err
	}
	return r.Operation(runID)
}

func (r *Runtime) GrantPassage(runID, stationID string) (model.OperationRecord, error) {
	control := r.ensureControl()
	if _, ok := control.Stations[stationID]; !ok {
		return model.OperationRecord{}, errors.New("unknown passage station")
	}
	generation := r.Tracker.Generation(runID)
	basis := r.Lineage.Basis(runID, stationID, generation, passageRequired, passageGap)
	if len(basis.Evidence) < passageRequired {
		return model.OperationRecord{}, errors.New("insufficient passage evidence")
	}
	if !basis.Continuous {
		return model.OperationRecord{}, errors.New("passage evidence is not continuous")
	}
	if !basis.Quorum || !basis.Independent {
		return model.OperationRecord{}, errors.New("passage evidence does not have independent physical sources")
	}
	proof := basis.Evidence[len(basis.Evidence)-1]
	if !permit.EvaluateRepressurization(r.Journal, r.Permits, proof) {
		return model.OperationRecord{}, errors.New("passage proof was not durable")
	}
	now := time.Now().UTC()
	r.Lineage.RecordPassageGrant(runID, stationID, generation, proof.ObservationID, passageRequired, passageGap, now)
	permitState := model.PermitState{
		RunID:      runID,
		StationID:  stationID,
		Name:       "repressurization",
		Generation: proof.Generation,
		Granted:    true,
		Reason:     "continuous durable passage evidence",
		UpdatedAt:  now,
	}
	r.mu.Lock()
	r.control.Permits[runID] = append(r.control.Permits[runID], permitState)
	r.control.Revision = model.NewRevision()
	r.mu.Unlock()
	if err := r.appendEvent(model.Event{
		RunID:   runID,
		Kind:    "passage_granted",
		Epoch:   generation,
		Payload: fmt.Sprintf("%s/%s", stationID, proof.ObservationID),
		At:      now,
	}); err != nil {
		return model.OperationRecord{}, err
	}
	return r.Operation(runID)
}

func (r *Runtime) Receive(runID string, request ReceiveRequest) (model.OperationRecord, error) {
	control := r.ensureControl()
	station, ok := control.Stations[request.StationID]
	if !ok || station.Role != model.StationReceiver {
		return model.OperationRecord{}, errors.New("receiver station is invalid")
	}
	current, ok := r.Runs.Get(runID)
	if !ok {
		return model.OperationRecord{}, errors.New("run not found")
	}
	if current.State != model.StateTracking {
		return model.OperationRecord{}, fmt.Errorf("run state %s cannot enter receiver", current.State)
	}
	r.Valves.BeginIsolation(runID, request.IsolationEpoch)
	r.Valves.CompleteIsolation(runID, request.IsolationEpoch)
	receiver.Prepare(r.Receiver, runID, request.IsolationEpoch)
	proof := r.Valves.IsolationProof(runID, request.IsolationEpoch)
	if err := r.Receiver.Open(runID, proof); err != nil {
		return model.OperationRecord{}, err
	}
	if err := run.Receive(r.Runs, runID); err != nil {
		return model.OperationRecord{}, err
	}
	if err := emergency.Execute(r.Valves, r.Pressure, runID, request.IsolationEpoch); err != nil {
		return model.OperationRecord{}, err
	}
	r.mu.Lock()
	pressureState := r.control.Pressures[request.StationID]
	pressureState.RunID = runID
	pressureState.Value = 0
	pressureState.Target = 0
	pressureState.Mode = model.CompressorStopped
	pressureState.ObservedAt = time.Now().UTC()
	r.control.Pressures[request.StationID] = pressureState
	valveState := r.control.Valves[request.StationID+"/main"]
	valveState.RunID = runID
	valveState.CommandEpoch = request.IsolationEpoch
	valveState.Position = model.ValveClosed
	valveState.ObservedAt = time.Now().UTC()
	r.control.Valves[valveState.ID] = valveState
	r.control.Revision = model.NewRevision()
	r.mu.Unlock()
	if err := r.appendEvent(model.Event{
		RunID:   runID,
		Kind:    "receiving",
		Epoch:   request.IsolationEpoch,
		Payload: request.StationID,
	}); err != nil {
		return model.OperationRecord{}, err
	}
	return r.Operation(runID)
}

func (r *Runtime) Complete(runID, routeID string) (model.OperationRecord, error) {
	if err := run.Complete(r.Runs, runID); err != nil {
		return model.OperationRecord{}, err
	}
	r.Routes.Release(routeID, runID)
	r.mu.Lock()
	if routeState, ok := r.control.Routes[routeID]; ok && routeState.RunID == runID {
		routeState.RunID = ""
		routeState.Reserved = false
		routeState.LeaseEpoch = 0
		routeState.UpdatedAt = time.Now().UTC()
		r.control.Routes[routeID] = routeState
	}
	r.control.Revision = model.NewRevision()
	r.mu.Unlock()
	if err := r.appendEvent(model.Event{
		RunID:   runID,
		Kind:    "complete",
		Payload: routeID,
	}); err != nil {
		return model.OperationRecord{}, err
	}
	return r.Operation(runID)
}

func (r *Runtime) Cancel(runID string) error {
	if err := r.Runs.Cancel(runID); err != nil {
		return err
	}
	r.Pressure.Stop()
	r.mu.Lock()
	for id, routeState := range r.control.Routes {
		if routeState.RunID == runID {
			r.Routes.Release(id, runID)
			routeState.RunID = ""
			routeState.Reserved = false
			routeState.LeaseEpoch = 0
			routeState.UpdatedAt = time.Now().UTC()
			r.control.Routes[id] = routeState
		}
	}
	r.control.Revision = model.NewRevision()
	r.mu.Unlock()
	return r.appendEvent(model.Event{
		RunID:   runID,
		Kind:    "cancelled",
		Payload: "operator cancellation",
	})
}

func (r *Runtime) Operation(runID string) (model.OperationRecord, error) {
	snapshot := r.Snapshot()
	operation, ok := snapshot.Operation(runID)
	if !ok {
		return model.OperationRecord{}, fmt.Errorf("run %s not found", runID)
	}
	return operation, nil
}

func (r *Runtime) Snapshot() model.ControlSnapshot {
	control := r.ensureControl()
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot := model.ControlSnapshot{
		Revision:   control.Revision,
		CapturedAt: time.Now().UTC(),
		Stations:   make([]model.Station, 0, len(control.Stations)),
		Valves:     make([]model.ValveState, 0, len(control.Valves)),
		Closures:   make([]model.ClosureState, 0, len(control.Closures)),
		Pressures:  make([]model.PressureState, 0, len(control.Pressures)),
		Routes:     make([]model.RouteState, 0, len(control.Routes)),
		Tracking:   make([]model.TrackingState, 0),
		Permits:    make([]model.PermitState, 0),
		Runs:       r.Runs.Snapshot(),
		Incidents:  append([]model.Incident(nil), control.Incidents...),
	}
	for _, value := range control.Stations {
		snapshot.Stations = append(snapshot.Stations, value)
	}
	snapshot.Stations = snapshot.SortedStations()
	for _, value := range control.Valves {
		snapshot.Valves = append(snapshot.Valves, value)
	}
	for _, value := range control.Closures {
		snapshot.Closures = append(snapshot.Closures, value)
	}
	for _, value := range control.Pressures {
		snapshot.Pressures = append(snapshot.Pressures, value)
	}
	for _, value := range control.Routes {
		snapshot.Routes = append(snapshot.Routes, value)
	}
	for _, values := range control.Tracking {
		snapshot.Tracking = append(snapshot.Tracking, values...)
	}
	for _, values := range control.Permits {
		snapshot.Permits = append(snapshot.Permits, values...)
	}
	sort.Slice(snapshot.Valves, func(i, j int) bool { return snapshot.Valves[i].ID < snapshot.Valves[j].ID })
	sort.Slice(snapshot.Routes, func(i, j int) bool { return snapshot.Routes[i].ID < snapshot.Routes[j].ID })
	sort.Slice(snapshot.Tracking, func(i, j int) bool { return snapshot.Tracking[i].ObservedAt.Before(snapshot.Tracking[j].ObservedAt) })
	return snapshot
}

func (r *Runtime) PersistSnapshot() error {
	control := r.ensureControl()
	if control.Persistent == nil {
		return nil
	}
	snapshot := r.Snapshot()
	if err := snapshot.Validate(); err != nil {
		return err
	}
	return control.Persistent.WriteSnapshot(snapshot)
}

func (r *Runtime) RecoverPersistent() (RecoveryReport, error) {
	control := r.ensureControl()
	if control.Persistent == nil {
		return RecoveryReport{Ready: true}, nil
	}
	snapshot, err := control.Persistent.ReadSnapshot()
	if err != nil {
		return RecoveryReport{}, err
	}
	if err := snapshot.Control.Validate(); err != nil {
		return RecoveryReport{}, err
	}
	run.Restore(r.Runs, snapshot.Control.Runs)
	leasing := make([]model.RouteLease, 0)
	for _, routeState := range snapshot.Control.Routes {
		if routeState.Reserved {
			leasing = append(leasing, model.RouteLease{
				Route:      routeState.ID,
				RunID:      routeState.RunID,
				Epoch:      routeState.LeaseEpoch,
				AcquiredAt: routeState.UpdatedAt,
			})
		}
	}
	route.Restore(r.Routes, leasing)
	permitEvents := make([]model.Event, 0)
	for _, permitState := range snapshot.Control.Permits {
		if permitState.Granted {
			permitEvents = append(permitEvents, model.Event{
				RunID:   permitState.RunID,
				Kind:    "passage",
				Epoch:   permitState.Generation,
				Payload: permitState.Reason,
				Durable: true,
			})
		}
	}
	permit.Restore(r.Permits, permitEvents)
	r.mu.Lock()
	r.control.Stations = snapshot.Control.StationIndex()
	r.control.Routes = make(map[string]model.RouteState)
	r.control.Valves = make(map[string]model.ValveState)
	r.control.Closures = make(map[string]model.ClosureState)
	r.control.Pressures = make(map[string]model.PressureState)
	r.control.Tracking = make(map[string][]model.TrackingState)
	r.control.Permits = make(map[string][]model.PermitState)
	for _, value := range snapshot.Control.Routes {
		r.control.Routes[value.ID] = value
	}
	for _, value := range snapshot.Control.Valves {
		r.control.Valves[value.ID] = value
	}
	for _, value := range snapshot.Control.Closures {
		r.control.Closures[value.StationID] = value
	}
	for _, value := range snapshot.Control.Pressures {
		r.control.Pressures[value.StationID] = value
	}
	for _, value := range snapshot.Control.Tracking {
		r.control.Tracking[value.RunID] = append(r.control.Tracking[value.RunID], value)
	}
	for _, value := range snapshot.Control.Permits {
		r.control.Permits[value.RunID] = append(r.control.Permits[value.RunID], value)
	}
	r.control.Incidents = append([]model.Incident(nil), snapshot.Control.Incidents...)
	r.control.Revision = model.NewRevision()
	r.mu.Unlock()
	events, err := control.Persistent.Events()
	if err != nil {
		return RecoveryReport{}, err
	}
	return RecoveryReport{
		RecoveredRuns:    len(snapshot.Control.Runs),
		RecoveredRoutes:  len(leasing),
		RecoveredPermits: len(permitEvents),
		JournalEvents:    len(events),
		Ready:            true,
	}, nil
}

func (r *Runtime) InterlockStatus() []model.Interlock {
	snapshot := r.Snapshot()
	interlocks := make([]model.Interlock, 0)
	for _, routeState := range snapshot.Routes {
		ready := !routeState.Reserved || routeState.RunID != ""
		reason := "route is available"
		if routeState.Reserved {
			reason = fmt.Sprintf("fenced to %s at epoch %d", routeState.RunID, routeState.LeaseEpoch)
		}
		interlocks = append(interlocks, model.Interlock{Name: "route/" + routeState.ID, Ready: ready, Reason: reason})
	}
	for _, pressureState := range snapshot.Pressures {
		station := snapshot.StationIndex()[pressureState.StationID]
		ready := pressureState.Value <= station.PressureLimit
		reason := fmt.Sprintf("pressure %.2f of limit %.2f", pressureState.Value, station.PressureLimit)
		interlocks = append(interlocks, model.Interlock{Name: "pressure/" + pressureState.StationID, Ready: ready, Reason: reason})
	}
	for _, closure := range snapshot.Closures {
		reason := "closure is not locked"
		if closure.Locked {
			reason = fmt.Sprintf("locked for %s at epoch %d", closure.RunID, closure.CommandEpoch)
		}
		interlocks = append(interlocks, model.Interlock{Name: "closure/" + closure.StationID, Ready: closure.Locked, Reason: reason})
	}
	sort.Slice(interlocks, func(i, j int) bool { return interlocks[i].Name < interlocks[j].Name })
	return interlocks
}

func (r *Runtime) EquipmentStatus() []model.Equipment {
	snapshot := r.Snapshot()
	equipment := make([]model.Equipment, 0, len(snapshot.Valves)+len(snapshot.Pressures)+len(snapshot.Closures))
	for _, valveState := range snapshot.Valves {
		equipment = append(equipment, model.Equipment{
			Name:      valveState.ID,
			Kind:      "valve",
			State:     string(valveState.Position),
			UpdatedAt: valveState.ObservedAt,
		})
	}
	for _, pressureState := range snapshot.Pressures {
		equipment = append(equipment, model.Equipment{
			Name:      pressureState.StationID + "/compressor",
			Kind:      "compressor",
			State:     string(pressureState.Mode),
			UpdatedAt: pressureState.ObservedAt,
		})
	}
	for _, closure := range snapshot.Closures {
		state := "unlocked"
		if closure.Locked {
			state = "locked"
		}
		equipment = append(equipment, model.Equipment{
			Name:      closure.StationID + "/closure",
			Kind:      "closure",
			State:     state,
			UpdatedAt: closure.ObservedAt,
		})
	}
	sort.Slice(equipment, func(i, j int) bool { return equipment[i].Name < equipment[j].Name })
	return equipment
}

func (r *Runtime) HealthDetails() map[string]any {
	snapshot := r.Snapshot()
	validationError := snapshot.Validate()
	sequence := int64(0)
	checksum := ""
	if r.control.Persistent != nil {
		sequence, checksum = r.control.Persistent.Tail()
	}
	ready := validationError == nil && run.Health(r.Runs) && tracker.Ready(r.Tracker) && launcher.Ready(r.Launcher)
	recoverySteps := NewRecoveryPlan().Steps()
	return map[string]any{
		"status":              map[bool]string{true: "ok", false: "degraded"}[ready],
		"ready":               ready,
		"revision":            snapshot.Revision,
		"active_runs":         len(run.Active(r.Runs)),
		"journal_events":      len(r.Journal.Events()),
		"persistent_sequence": sequence,
		"persistent_checksum": checksum,
		"validation_error":    errorText(validationError),
		"route_leases":        len(route.Snapshot(r.Routes)),
		"emergency_safe":      emergency.Safe(r.Pressure),
		"pressure_samples":    len(r.Pressure.History()),
		"recovery_steps":      recoverySteps,
		"captured_at":         snapshot.CapturedAt,
	}
}

func (r *Runtime) ReviseTracking(runID string) (int64, error) {
	if _, ok := r.Runs.Get(runID); !ok {
		return 0, fmt.Errorf("run %s not found", runID)
	}
	generation := r.Tracker.Revise(runID)
	if err := r.appendEvent(model.Event{RunID: runID, Kind: "tracking_revision", Epoch: generation}); err != nil {
		return 0, err
	}
	r.mu.Lock()
	r.control.Revision = model.NewRevision()
	r.mu.Unlock()
	return generation, nil
}

// WithdrawObservation retracts a mistyped probe observation. The original
// observation and the correction relationship are retained, the passage basis
// (physical-source independence and continuity) is recomputed from the
// remaining observations without bumping the generation, and any passage
// decision that relied on the withdrawn source is flagged for review.
func (r *Runtime) WithdrawObservation(runID string, request WithdrawObservationRequest) (LineageReport, error) {
	observationID := strings.TrimSpace(request.ObservationID)
	if observationID == "" {
		return LineageReport{}, errors.New("observation id is required")
	}
	if _, ok := r.Runs.Get(runID); !ok {
		return LineageReport{}, fmt.Errorf("run %s not found", runID)
	}
	observation, ok := r.Lineage.Get(observationID)
	if !ok {
		return LineageReport{}, errors.New("observation does not exist")
	}
	if observation.RunID != runID {
		return LineageReport{}, errors.New("observation belongs to a different run")
	}
	if observation.Status == tracker.ObservationWithdrawn {
		return LineageReport{}, errors.New("observation has already been withdrawn")
	}
	reason := strings.TrimSpace(request.Reason)
	if reason == "" {
		reason = "operator withdrawal"
	}
	withdrawnAt := time.Now().UTC()
	withdrawn, withdrawnIDs, err := r.Lineage.Withdraw(observationID, reason, withdrawnAt)
	if err != nil {
		return LineageReport{}, err
	}
	// Remove the live points so Clearance/quorum recompute over the rest of
	// the same generation.
	for _, id := range withdrawnIDs {
		r.Tracker.Retract(id)
	}
	reviewReason := fmt.Sprintf("observation %s withdrawn: %s", observationID, reason)
	affected := r.Lineage.MarkAffectedReviews(runID, withdrawnIDs, reviewReason, passageRequired, passageGap)
	r.markPermitsForReview(runID, affected, reviewReason)
	payload := fmt.Sprintf("%s/%s/%s", withdrawn.StationID, observationID, reason)
	if err := r.appendEvent(model.Event{
		RunID:   runID,
		Kind:    "tracking_withdrawal",
		Epoch:   withdrawn.Generation,
		Payload: payload,
		At:      withdrawnAt,
	}); err != nil {
		return LineageReport{}, err
	}
	return r.buildLineageReport(runID), nil
}

// LineageReport returns the preserved observation history, correction ledger,
// recomputed bases and review flags for a run.
func (r *Runtime) LineageReport(runID string) (LineageReport, error) {
	if _, ok := r.Runs.Get(runID); !ok {
		return LineageReport{}, fmt.Errorf("run %s not found", runID)
	}
	return r.buildLineageReport(runID), nil
}

func (r *Runtime) buildLineageReport(runID string) LineageReport {
	observations := r.Lineage.RunHistory(runID)
	seen := make(map[string]bool)
	bases := make([]tracker.PassageBasis, 0)
	for _, observation := range observations {
		key := observation.StationID + "/" + strconv.FormatInt(observation.Generation, 10)
		if seen[key] {
			continue
		}
		seen[key] = true
		bases = append(bases, r.Lineage.Basis(runID, observation.StationID, observation.Generation, passageRequired, passageGap))
	}
	sort.Slice(bases, func(i, j int) bool {
		if bases[i].StationID == bases[j].StationID {
			return bases[i].Generation < bases[j].Generation
		}
		return bases[i].StationID < bases[j].StationID
	})
	return LineageReport{
		RunID:          runID,
		Observations:   observations,
		Corrections:    r.Lineage.CorrectionsForRun(runID),
		Bases:          bases,
		Reviews:        r.Lineage.Reviews(runID),
		ReviewRequired: r.Lineage.ReviewRequired(runID),
	}
}

// markPermitsForReview flags granted permits whose recorded basis intersects
// a withdrawn observation. The permits are not revoked: they await operator
// re-check.
func (r *Runtime) markPermitsForReview(runID string, affected []tracker.PassageReview, reason string) {
	if len(affected) == 0 {
		return
	}
	keys := make(map[string]bool, len(affected))
	for _, review := range affected {
		keys[review.StationID+"/"+strconv.FormatInt(review.Generation, 10)] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, permitState := range r.control.Permits[runID] {
		if !permitState.Granted || permitState.ReviewRequired {
			continue
		}
		if keys[permitState.StationID+"/"+strconv.FormatInt(permitState.Generation, 10)] {
			permitState.ReviewRequired = true
			permitState.ReviewReason = reason
			permitState.UpdatedAt = time.Now().UTC()
			r.control.Permits[runID][i] = permitState
		}
	}
	r.control.Revision = model.NewRevision()
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
