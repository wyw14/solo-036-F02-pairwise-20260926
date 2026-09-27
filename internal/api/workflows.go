package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wyw14/solo-036/internal/service"
	"github.com/wyw14/solo-036/internal/tracker"
)

const requestLimit = 1024 * 1024

type errorResponse struct {
	Error     string `json:"error"`
	RequestID string `json:"request_id,omitempty"`
}

type createRequest struct {
	StationID      string  `json:"station_id"`
	RouteID        string  `json:"route_id"`
	TargetPressure float64 `json:"target_pressure"`
}

type trackRequest struct {
	StationID             string  `json:"station_id"`
	SourceID              string  `json:"source_id"`
	Kind                  string  `json:"kind"`
	Position              float64 `json:"position"`
	ObservedAt            string  `json:"observed_at"`
	CorrectsObservationID string  `json:"corrects_observation_id"`
}

type withdrawObservationRequest struct {
	ObservationID string `json:"observation_id"`
	Reason        string `json:"reason"`
}

type receiveRequest struct {
	StationID      string `json:"station_id"`
	IsolationEpoch int64  `json:"isolation_epoch"`
}

type completeRequest struct {
	RouteID string `json:"route_id"`
}

type passageRequest struct {
	StationID string `json:"station_id"`
}

func decodeBody(w http.ResponseWriter, r *http.Request, destination any) error {
	reader := http.MaxBytesReader(w, r.Body, requestLimit)
	defer reader.Close()
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("request body is required")
		}
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON value")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, err error) {
	requestID := strings.TrimSpace(r.Header.Get("x-request-id"))
	writeJSON(w, status, errorResponse{Error: err.Error(), RequestID: requestID})
}

func parseObservedAt(value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Now().UTC(), nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, errors.New("observed_at must use RFC3339 format")
	}
	return parsed.UTC(), nil
}

func (h *Handler) Launch(w http.ResponseWriter, r *http.Request) {
	var request createRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}
	operation, err := h.runtime.Launch(r.Context(), service.LaunchRequest{
		StationID:      request.StationID,
		RouteID:        request.RouteID,
		TargetPressure: request.TargetPressure,
	})
	if err != nil {
		writeError(w, r, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusCreated, operation)
}

func (h *Handler) Operation(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	operation, err := h.runtime.Operation(runID)
	if err != nil {
		writeError(w, r, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, operation)
}

func (h *Handler) Track(w http.ResponseWriter, r *http.Request) {
	var request trackRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}
	observedAt, err := parseObservedAt(request.ObservedAt)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}
	operation, err := h.runtime.Track(chi.URLParam(r, "runID"), service.TrackingRequest{
		StationID:             request.StationID,
		SourceID:              request.SourceID,
		Kind:                  tracker.SourceKind(request.Kind),
		Position:              request.Position,
		ObservedAt:            observedAt,
		CorrectsObservationID: request.CorrectsObservationID,
	})
	if err != nil {
		writeError(w, r, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, operation)
}

func (h *Handler) WithdrawObservation(w http.ResponseWriter, r *http.Request) {
	var request withdrawObservationRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}
	report, err := h.runtime.WithdrawObservation(chi.URLParam(r, "runID"), service.WithdrawObservationRequest{
		ObservationID: request.ObservationID,
		Reason:        request.Reason,
	})
	if err != nil {
		writeError(w, r, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (h *Handler) Lineage(w http.ResponseWriter, r *http.Request) {
	report, err := h.runtime.LineageReport(chi.URLParam(r, "runID"))
	if err != nil {
		writeError(w, r, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (h *Handler) ReviseTracking(w http.ResponseWriter, r *http.Request) {
	generation, err := h.runtime.ReviseTracking(chi.URLParam(r, "runID"))
	if err != nil {
		writeError(w, r, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"generation": generation})
}

func (h *Handler) GrantPassage(w http.ResponseWriter, r *http.Request) {
	var request passageRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}
	operation, err := h.runtime.GrantPassage(chi.URLParam(r, "runID"), request.StationID)
	if err != nil {
		writeError(w, r, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, operation)
}

func (h *Handler) Receive(w http.ResponseWriter, r *http.Request) {
	var request receiveRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}
	operation, err := h.runtime.Receive(chi.URLParam(r, "runID"), service.ReceiveRequest{
		StationID:      request.StationID,
		IsolationEpoch: request.IsolationEpoch,
	})
	if err != nil {
		writeError(w, r, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, operation)
}

func (h *Handler) Complete(w http.ResponseWriter, r *http.Request) {
	var request completeRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}
	operation, err := h.runtime.Complete(chi.URLParam(r, "runID"), request.RouteID)
	if err != nil {
		writeError(w, r, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, operation)
}

func (h *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	if err := h.runtime.Cancel(chi.URLParam(r, "runID")); err != nil {
		writeError(w, r, http.StatusConflict, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) Snapshot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.runtime.Snapshot())
}
