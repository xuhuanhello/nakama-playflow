package fleetmanager

import (
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"

	"github.com/xuhuanhello/nakama-playflow/internal/state"
)

type BootstrapRequest struct {
	WorkerID       string `json:"worker_id"`
	BootstrapToken string `json:"bootstrap_token"`
	BootID         string `json:"boot_id"`
	BuildHash      string `json:"build_hash"`
}
type RoomReport struct {
	RoomID  string   `json:"room_id"`
	State   string   `json:"state"`
	UserIDs []string `json:"user_ids"`
}
type CommandResult struct {
	CommandID string `json:"command_id"`
	Success   bool   `json:"success"`
	Error     string `json:"error"`
}
type HeartbeatRequest struct {
	WorkerID       string          `json:"worker_id"`
	BootID         string          `json:"boot_id"`
	Sequence       uint64          `json:"sequence"`
	Ready          bool            `json:"ready"`
	Rooms          []RoomReport    `json:"rooms"`
	Metrics        state.Metrics   `json:"metrics"`
	CommandResults []CommandResult `json:"command_results"`
}
type HeartbeatResponse struct {
	Commands []*state.Command `json:"commands"`
	Draining bool             `json:"draining"`
	Revision int64            `json:"revision"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func decodeBody(w http.ResponseWriter, r *http.Request, value any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return fmt.Errorf("invalid_request")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("invalid_request")
	}
	return nil
}
func failHTTP(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "internal_error"
	switch {
	case errors.Is(err, ErrForbidden):
		status, code = 403, "forbidden"
	case errors.Is(err, ErrNotFound):
		status, code = 404, "not_found"
	case errors.Is(err, ErrBusy):
		status, code = 409, "unavailable"
	}
	writeJSON(w, status, map[string]string{"error": code})
}
func equalToken(got, want string) bool { return len(got) > 0 && hmac.Equal([]byte(got), []byte(want)) }
func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// HTTPHandler is registered on Nakama's existing HTTP server. All non-public
// routes authenticate here; they do not inherit an assumption of Nakama auth.
func (m *Manager) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /fleet/v1/agent/bootstrap", m.bootstrapHTTP)
	mux.HandleFunc("POST /fleet/v1/agent/heartbeat", m.heartbeatHTTP)
	mux.HandleFunc("POST /fleet/v1/admin/retry-creation", m.admin(func(w http.ResponseWriter, r *http.Request) {
		if err := m.store.Update(r.Context(), func(s *state.State) error { s.CreationBlockedReason = ""; s.NextCreateAt = 0; return nil }); err != nil {
			failHTTP(w, err)
			return
		}
		writeJSON(w, 202, map[string]bool{"accepted": true})
	}))
	mux.HandleFunc("GET /fleet/v1/admin/status", m.admin(func(w http.ResponseWriter, r *http.Request) {
		s, err := m.store.View(r.Context())
		if err != nil {
			failHTTP(w, err)
			return
		}
		writeJSON(w, 200, s)
	}))
	mux.HandleFunc("POST /fleet/v1/admin/drain", m.admin(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			WorkerID string `json:"worker_id"`
		}
		if err := decodeBody(w, r, &input); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid_request"})
			return
		}
		if err := m.Drain(r.Context(), input.WorkerID); err != nil {
			failHTTP(w, err)
			return
		}
		writeJSON(w, 202, map[string]bool{"accepted": true})
	}))
	if m.cfg.Mode == "mock" {
		mux.HandleFunc("POST /fleet/v1/admin/allocate", m.admin(func(w http.ResponseWriter, r *http.Request) {
			var input struct {
				UserIDs    []string `json:"user_ids"`
				RequestKey string   `json:"request_key"`
			}
			if err := decodeBody(w, r, &input); err != nil || !validUsers(input.UserIDs) {
				writeJSON(w, 400, map[string]string{"error": "invalid_request"})
				return
			}
			allocationID, err := m.Allocate(r.Context(), input.UserIDs, input.RequestKey)
			if err != nil {
				failHTTP(w, err)
				return
			}
			writeJSON(w, 202, map[string]string{"allocation_id": allocationID})
		}))
	}
	return mux
}
func (m *Manager) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !equalToken(bearer(r), m.cfg.AdminToken) {
			writeJSON(w, 403, map[string]string{"error": "forbidden"})
			return
		}
		next(w, r)
	}
}
func (m *Manager) bootstrapHTTP(w http.ResponseWriter, r *http.Request) {
	var input BootstrapRequest
	if err := decodeBody(w, r, &input); err != nil || input.WorkerID == "" || input.BootID == "" || len(input.BootID) > 128 {
		writeJSON(w, 400, map[string]string{"error": "invalid_request"})
		return
	}
	if !equalToken(input.BootstrapToken, encoded(derive(m.cfg.SigningKey, "bootstrap:"+input.WorkerID))) || input.BuildHash != m.cfg.BuildHash {
		failHTTP(w, ErrForbidden)
		return
	}
	err := m.store.Update(r.Context(), func(s *state.State) error {
		worker := s.Workers[input.WorkerID]
		if worker == nil {
			return ErrNotFound
		}
		if worker.State == "stopped" || worker.State == "failed" || worker.State == "lost" {
			return ErrBusy
		}
		if worker.BootID != "" && worker.BootID != input.BootID {
			return ErrForbidden
		}
		worker.BootID = input.BootID
		return nil
	})
	if err != nil {
		failHTTP(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"agent_token": encoded(derive(m.cfg.SigningKey, "agent:"+input.WorkerID+":"+input.BootID)), "heartbeat_interval_seconds": 2})
}
func validMetrics(metrics state.Metrics) bool {
	return metrics.SimulationPending >= 0 && metrics.SimulationActive >= 0 && metrics.AuditActive >= 0 && metrics.AuditPending >= 0 && metrics.PendingResults >= 0 && metrics.MemoryBytes >= 0 && metrics.SimulationOldestSeconds >= 0 && !math.IsNaN(metrics.SimulationOldestSeconds) && !math.IsInf(metrics.SimulationOldestSeconds, 0) && metrics.FrameP99MS >= 0 && !math.IsNaN(metrics.FrameP99MS) && !math.IsInf(metrics.FrameP99MS, 0)
}

func (m *Manager) heartbeatHTTP(w http.ResponseWriter, r *http.Request) {
	var input HeartbeatRequest
	if err := decodeBody(w, r, &input); err != nil || input.Sequence == 0 || len(input.Rooms) > m.cfg.MaxRooms || len(input.CommandResults) > 256 || !validMetrics(input.Metrics) {
		writeJSON(w, 400, map[string]string{"error": "invalid_request"})
		return
	}
	if !equalToken(bearer(r), encoded(derive(m.cfg.SigningKey, "agent:"+input.WorkerID+":"+input.BootID))) {
		failHTTP(w, ErrForbidden)
		return
	}
	response := HeartbeatResponse{Commands: []*state.Command{}}
	err := m.store.Update(r.Context(), func(s *state.State) error {
		worker := s.Workers[input.WorkerID]
		if worker == nil {
			return ErrNotFound
		}
		if worker.BootID != input.BootID || worker.State == "stopped" || worker.State == "lost" || worker.State == "failed" {
			return ErrForbidden
		}
		now := m.now().Unix()
		if input.Sequence > worker.LastSequence {
			seenRooms := map[string]bool{}
			for _, room := range input.Rooms {
				if seenRooms[room.RoomID] || len(room.UserIDs) > 2 {
					return ErrForbidden
				}
				seenRooms[room.RoomID] = true
				var a *state.Allocation
				for _, candidate := range s.Allocations {
					if candidate.RoomID == room.RoomID && candidate.WorkerID == worker.ID {
						a = candidate
						break
					}
				}
				if a == nil {
					return ErrForbidden
				}
				if !validRoomState(room.State) {
					return ErrForbidden
				}
				seenUsers := map[string]bool{}
				for _, user := range room.UserIDs {
					if !contains(a.UserIDs, user) || seenUsers[user] {
						return ErrForbidden
					}
					seenUsers[user] = true
				}
			}
			worker.LastHeartbeat = now
			worker.LastSequence = input.Sequence
			worker.Ready = input.Ready
			worker.Metrics = input.Metrics
			if worker.Host != "" && input.Ready && worker.State != "stopping" && worker.State != "unknown" && worker.State != "requested" && worker.State != "starting" {
				if worker.Draining {
					worker.State = "draining"
				} else {
					worker.State = "ready"
				}
			}
			for _, result := range input.CommandResults {
				command := s.Commands[result.CommandID]
				if command == nil || command.WorkerID != worker.ID {
					return ErrForbidden
				}
				if command.Done {
					continue
				}
				if result.Success {
					command.Done = true
					if command.Type == "drain" {
						worker.DrainAck = true
					}
					a := s.Allocations[command.AllocationID]
					if a != nil && a.Epoch == command.Epoch {
						if command.Type == "prepare_room" && a.State == "preparing" {
							if command.ExpiresAt <= now || worker.Draining {
								m.cancel(s, a, "prepare_expired", now)
							} else {
								a.State = "prepared"
								touch(a)
								if m.eligible(worker, now) {
									if err := reserve(a, a.UserIDs, now, m.cfg.ReservationTTL); err != nil {
										return err
									}
								}
							}
						}
						if command.Type == "cancel_room" && a.State == "cancelling" {
							phase := "expired"
							if a.Error == "cancelled" {
								phase = "cancelled"
							}
							finish(a, phase, a.Error, now)
						}
					}
				} else if command.Type == "prepare_room" {
					command.Done = true
					if a := s.Allocations[command.AllocationID]; a != nil && a.State == "preparing" {
						m.cancel(s, a, "room_prepare_failed", now)
					}
				} else {
					// A command ID denotes one execution attempt. Agent transport
					// retries replay the same result; retry execution with a new ID.
					command.Done = true
					retry := *command
					retry.ID = id()
					retry.Done = false
					retry.Attempts++
					delay := int64(2 << min(retry.Attempts, 4))
					retry.NotBefore = now + delay
					retry.ExpiresAt = now + 3600
					s.Commands[retry.ID] = &retry
				}
			}
			worker.PlayerCount = 0
			for _, room := range input.Rooms {
				for _, a := range s.Allocations {
					if a.RoomID != room.RoomID || a.WorkerID != worker.ID || state.Terminal(a.State) {
						continue
					}
					if room.State == "closed" {
						if a.State == "active" || a.State == "assigned" {
							finish(a, "completed", "", now)
						}
						continue
					}
					if a.State == "assigned" || a.State == "active" {
						for i := range a.Sessions {
							session := &a.Sessions[i]
							connected := contains(room.UserIDs, session.UserID)
							if connected {
								session.EverConnected = true
								session.ReconnectUntil = 0
							} else if session.Connected && session.ReconnectUntil == 0 {
								session.ReconnectUntil = now + m.cfg.ReconnectSeconds
							}
							session.Connected = connected
						}
						if room.State == "playing" && len(room.UserIDs) == len(a.UserIDs) && a.State == "assigned" {
							a.State = "active"
							touch(a)
						}
					}
					worker.PlayerCount += len(room.UserIDs)
				}
			}
		}
		for _, command := range s.Commands {
			if command.WorkerID == worker.ID && !command.Done && command.NotBefore <= now {
				copy := *command
				response.Commands = append(response.Commands, &copy)
			}
		}
		sort.Slice(response.Commands, func(i, j int) bool { return response.Commands[i].ID < response.Commands[j].ID })
		if len(response.Commands) > 64 {
			response.Commands = response.Commands[:64]
		}
		response.Draining = worker.Draining
		response.Revision = s.Revision + 1
		return nil
	})
	if err != nil {
		failHTTP(w, err)
		return
	}
	writeJSON(w, 200, response)
}
func validRoomState(value string) bool {
	switch value {
	case "waiting_players", "playing", "closing", "closed", "between_rounds":
		return true
	}
	return false
}
