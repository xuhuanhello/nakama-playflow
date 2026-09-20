// Package state owns durable control-plane state. No provider IO is performed in a transaction.
package state

import (
	"context"
	"encoding/json"
	"sync"
)

type State struct {
	Profile               string                 `json:"profile"`
	Revision              int64                  `json:"revision"`
	Leader                string                 `json:"leader"`
	LeaseUntil            int64                  `json:"lease_until"`
	NextCreateAt          int64                  `json:"next_create_at"`
	CreationBlockedReason string                 `json:"creation_blocked_reason,omitempty"`
	Workers               map[string]*Worker     `json:"workers"`
	Allocations           map[string]*Allocation `json:"allocations"`
	Commands              map[string]*Command    `json:"commands"`
}

type Worker struct {
	ID                  string  `json:"id"`
	ProviderID          string  `json:"provider_id"`
	ProviderExpiresAt   int64   `json:"provider_expires_at,omitempty"`
	BootID              string  `json:"boot_id"`
	State               string  `json:"state"`
	Region              string  `json:"region"`
	BuildHash           string  `json:"build_hash"`
	Host                string  `json:"host"`
	Port                int     `json:"port"`
	MaxRooms            int     `json:"max_rooms"`
	CreatedAt           int64   `json:"created_at"`
	UpdatedAt           int64   `json:"updated_at"`
	LastHeartbeat       int64   `json:"last_heartbeat"`
	LastSequence        uint64  `json:"last_sequence"`
	NextCheckAt         int64   `json:"next_check_at"`
	IdleSince           int64   `json:"idle_since"`
	Ready               bool    `json:"ready"`
	Draining            bool    `json:"draining"`
	DrainAck            bool    `json:"drain_ack"`
	PlayerCount         int     `json:"player_count"`
	Metrics             Metrics `json:"metrics"`
	Error               string  `json:"error,omitempty"`
	InitialAllocationID string  `json:"initial_allocation_id,omitempty"`
}

type Metrics struct {
	SimulationPending       int     `json:"simulation_pending"`
	SimulationActive        int     `json:"simulation_active"`
	SimulationOldestSeconds float64 `json:"simulation_oldest_seconds"`
	FrameP99MS              float64 `json:"frame_p99_ms"`
	MemoryBytes             int64   `json:"memory_bytes"`
	AuditPending            int     `json:"audit_pending"`
	AuditActive             int     `json:"audit_active"`
	PendingResults          int     `json:"pending_results"`
}

type Allocation struct {
	ID                string    `json:"allocation_id"`
	RequestKey        string    `json:"request_key"`
	RoomID            string    `json:"room_id"`
	WorkerID          string    `json:"worker_id"`
	PreferredWorkerID string    `json:"preferred_worker_id,omitempty"`
	UserIDs           []string  `json:"user_ids"`
	State             string    `json:"state"`
	Epoch             int64     `json:"epoch"`
	Revision          int64     `json:"revision"`
	CreatedAt         int64     `json:"created_at"`
	ExpiresAt         int64     `json:"expires_at"`
	Sessions          []Session `json:"sessions"`
	Error             string    `json:"error,omitempty"`
	TerminalAt        int64     `json:"terminal_at,omitempty"`
	NotifiedRevision  int64     `json:"notified_revision"`
}

type Session struct {
	ID             string `json:"reservation_id"`
	UserID         string `json:"user_id"`
	Seat           int    `json:"seat"`
	ExpiresAt      int64  `json:"expires_at"`
	Connected      bool   `json:"connected"`
	EverConnected  bool   `json:"ever_connected"`
	ReconnectUntil int64  `json:"reconnect_until"`
}

type Command struct {
	ID           string   `json:"command_id"`
	Type         string   `json:"type"`
	WorkerID     string   `json:"worker_id,omitempty"`
	RoomID       string   `json:"room_id,omitempty"`
	AllocationID string   `json:"allocation_id,omitempty"`
	Epoch        int64    `json:"epoch"`
	UserIDs      []string `json:"user_ids,omitempty"`
	ExpiresAt    int64    `json:"expires_at"`
	Done         bool     `json:"done,omitempty"`
	Attempts     int      `json:"attempts,omitempty"`
	NotBefore    int64    `json:"not_before,omitempty"`
}

func New() *State {
	return &State{Workers: map[string]*Worker{}, Allocations: map[string]*Allocation{}, Commands: map[string]*Command{}}
}
func (s *State) Normalize() {
	if s.Workers == nil {
		s.Workers = map[string]*Worker{}
	}
	if s.Allocations == nil {
		s.Allocations = map[string]*Allocation{}
	}
	if s.Commands == nil {
		s.Commands = map[string]*Command{}
	}
}
func Terminal(phase string) bool {
	return phase == "completed" || phase == "cancelled" || phase == "expired" || phase == "failed"
}
func LiveWorker(phase string) bool { return phase != "stopped" && phase != "failed" && phase != "lost" }
func Occupied(s *State, workerID string) int {
	n := 0
	for _, a := range s.Allocations {
		if a.WorkerID == workerID && !Terminal(a.State) {
			n++
		}
	}
	return n
}

type Store interface {
	View(context.Context) (*State, error)
	Update(context.Context, func(*State) error) error
}

// Memory uses copy-on-write to preserve transaction rollback semantics in tests.
type Memory struct {
	mu    sync.Mutex
	state *State
}

func NewMemory() *Memory { return &Memory{state: New()} }
func clone(s *State) *State {
	b, _ := json.Marshal(s)
	result := New()
	_ = json.Unmarshal(b, result)
	return result
}
func (m *Memory) View(ctx context.Context) (*State, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return clone(m.state), nil
}
func (m *Memory) Update(ctx context.Context, fn func(*State) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	next := clone(m.state)
	if err := fn(next); err != nil {
		return err
	}
	next.Revision++
	m.state = next
	return nil
}
