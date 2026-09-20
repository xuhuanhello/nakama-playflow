// Package fleetmanager implements Nakama's FleetManager with multi-room admission.
package fleetmanager

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/xuhuanhello/nakama-playflow/internal/playflow"
	"github.com/xuhuanhello/nakama-playflow/internal/state"
)

var ErrNotFound = errors.New("not_found")
var ErrBusy = errors.New("capacity_or_state_unavailable")
var ErrForbidden = errors.New("forbidden")

type Provider interface {
	Start(context.Context, playflow.StartRequest) (playflow.Instance, error)
	Get(context.Context, string) (playflow.Instance, error)
	List(context.Context) ([]playflow.Instance, error)
	Stop(context.Context, string) error
}

type Manager struct {
	cfg             Config
	store           state.Store
	provider        Provider
	now             func() time.Time
	owner           string
	tickMu          sync.Mutex
	callbackMu      sync.Mutex
	callbacks       map[string]string
	callbackHandler runtime.FmCallbackHandler
	nk              runtime.NakamaModule
	onStop          func()
}

var _ runtime.FleetManagerInitializer = (*Manager)(nil)

func New(cfg Config, store state.Store, provider Provider) (*Manager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if store == nil || provider == nil {
		return nil, fmt.Errorf("store and provider are required")
	}
	// One namespace is one immutable build/region pool. Refuse config drift
	// across controllers instead of assigning new clients to old game binaries.
	profileData, _ := json.Marshal([]any{cfg.BuildHash, cfg.Region, cfg.ProviderVersion, cfg.MaxRooms, cfg.PortName, cfg.ComputeSize, encoded(derive(cfg.SigningKey, "profile"))})
	profile := fmt.Sprintf("%x", sha256.Sum256(profileData))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.Update(ctx, func(s *state.State) error {
		if s.Profile != "" && s.Profile != profile {
			return fmt.Errorf("fleet namespace profile changed: use a new deployment ID for a new build, region or signing key")
		}
		for _, w := range s.Workers {
			if w.BuildHash != cfg.BuildHash || w.Region != cfg.Region {
				return fmt.Errorf("existing worker does not match fleet profile")
			}
		}
		s.Profile = profile
		return nil
	}); err != nil {
		return nil, err
	}
	return &Manager{cfg: cfg, store: store, provider: provider, now: time.Now, owner: id(), callbacks: map[string]string{}}, nil
}
func (m *Manager) Init(nk runtime.NakamaModule, handler runtime.FmCallbackHandler) error {
	m.callbackMu.Lock()
	defer m.callbackMu.Unlock()
	m.nk = nk
	m.callbackHandler = handler
	return nil
}
func (m *Manager) Config() Config                                     { return m.cfg }
func (m *Manager) Snapshot(ctx context.Context) (*state.State, error) { return m.store.View(ctx) }

func (m *Manager) Run(ctx context.Context, onError func(error)) {
	if m.onStop != nil {
		defer m.onStop()
	}
	t := time.NewTicker(m.cfg.Interval)
	defer t.Stop()
	for {
		if err := m.Tick(ctx); err != nil && ctx.Err() == nil && onError != nil {
			onError(err)
		}
		select {
		case <-ctx.Done():
			cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = m.store.Update(cleanup, func(s *state.State) error {
				if s.Leader == m.owner {
					s.LeaseUntil = 0
				}
				return nil
			})
			cancel()
			return
		case <-t.C:
		}
	}
}

func validUsers(users []string) bool {
	if len(users) != 2 {
		return false
	}
	return users[0] != "" && users[1] != "" && users[0] != users[1] && len(users[0]) <= 128 && len(users[1]) <= 128
}
func contains(users []string, user string) bool {
	for _, u := range users {
		if u == user {
			return true
		}
	}
	return false
}
func sameUsers(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]string{}, a...)
	bb := append([]string{}, b...)
	sort.Strings(aa)
	sort.Strings(bb)
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}
func touch(a *state.Allocation) { a.Revision++ }
func finish(a *state.Allocation, phase, reason string, now int64) {
	a.State = phase
	a.Error = reason
	a.TerminalAt = now
	touch(a)
}
func allocation(s *state.State, users []string, key string, now int64, timeout int64) (*state.Allocation, error) {
	if !validUsers(users) || key == "" || len(key) > 256 {
		return nil, fmt.Errorf("two distinct users and a bounded request key are required")
	}
	for _, a := range s.Allocations {
		if a.RequestKey == key {
			if !sameUsers(a.UserIDs, users) {
				return nil, ErrForbidden
			}
			return a, nil
		}
		if !state.Terminal(a.State) {
			for _, user := range users {
				if contains(a.UserIDs, user) {
					return nil, ErrBusy
				}
			}
		}
	}
	a := &state.Allocation{ID: id(), RequestKey: key, RoomID: id(), UserIDs: append([]string{}, users...), State: "waiting_capacity", Epoch: 1, Revision: 1, CreatedAt: now, ExpiresAt: now + timeout}
	s.Allocations[a.ID] = a
	return a, nil
}

func (m *Manager) Allocate(ctx context.Context, users []string, key string) (string, error) {
	var allocationID string
	err := m.store.Update(ctx, func(s *state.State) error {
		a, err := allocation(s, users, key, m.now().Unix(), m.cfg.AllocationTimeout)
		if err == nil {
			allocationID = a.ID
		}
		return err
	})
	return allocationID, err
}
func (m *Manager) Cancel(ctx context.Context, user, allocationID string) error {
	return m.store.Update(ctx, func(s *state.State) error {
		a, ok := s.Allocations[allocationID]
		if !ok {
			return ErrNotFound
		}
		if !contains(a.UserIDs, user) {
			return ErrForbidden
		}
		if state.Terminal(a.State) || a.State == "cancelling" {
			return nil
		}
		if a.State == "active" {
			return ErrBusy
		}
		m.cancel(s, a, "cancelled", m.now().Unix())
		return nil
	})
}
func (m *Manager) cancel(s *state.State, a *state.Allocation, reason string, now int64) {
	if state.Terminal(a.State) || a.State == "cancelling" {
		return
	}
	if a.WorkerID == "" {
		phase := "expired"
		if reason == "cancelled" {
			phase = "cancelled"
		}
		finish(a, phase, reason, now)
		return
	}
	a.State = "cancelling"
	a.Error = reason
	touch(a)
	// Withdraw undispatched preparation. Hosts also tombstone cancelled epochs,
	// because a previously delivered preparation may still be in flight.
	for _, command := range s.Commands {
		if command.AllocationID == a.ID && command.Type == "prepare_room" {
			command.Done = true
		}
	}
	c := &state.Command{ID: id(), Type: "cancel_room", WorkerID: a.WorkerID, RoomID: a.RoomID, AllocationID: a.ID, Epoch: a.Epoch, ExpiresAt: now + m.cfg.PrepareTimeout}
	s.Commands[c.ID] = c
}
func (m *Manager) newWorker(s *state.State, now int64, maxRooms int) *state.Worker {
	w := &state.Worker{ID: id(), State: "requested", Region: m.cfg.Region, BuildHash: m.cfg.BuildHash, MaxRooms: maxRooms, CreatedAt: now, UpdatedAt: now, NextCheckAt: now}
	s.Workers[w.ID] = w
	return w
}
func (m *Manager) liveCount(s *state.State) int {
	n := 0
	for _, w := range s.Workers {
		if w.State != "stopped" && !(w.State == "failed" && w.ProviderID == "") {
			n++
		}
	}
	return n
}

func (m *Manager) Create(ctx context.Context, maxPlayers int, users []string, latencies []runtime.FleetUserLatencies, metadata map[string]any, callback runtime.FmCreateCallbackFn) (map[string]string, error) {
	if maxPlayers < 2 || maxPlayers%2 != 0 || maxPlayers > m.cfg.MaxRooms*2 {
		return nil, fmt.Errorf("maxPlayers must be an even physical-instance capacity within this profile")
	}
	if len(users) > 0 && !validUsers(users) {
		return nil, fmt.Errorf("initial DM room requires exactly two distinct users")
	}
	for _, latency := range latencies {
		if latency.RegionIdentifier != m.cfg.Region {
			return nil, fmt.Errorf("this manager is configured for a single region")
		}
	}
	var workerID string
	err := m.store.Update(ctx, func(s *state.State) error {
		if m.liveCount(s) >= m.cfg.MaxInstances || s.CreationBlockedReason != "" {
			return ErrBusy
		}
		now := m.now().Unix()
		w := m.newWorker(s, now, maxPlayers/2)
		workerID = w.ID
		if len(users) > 0 {
			a, err := allocation(s, users, "create:"+w.ID, now, m.cfg.AllocationTimeout)
			if err != nil {
				return err
			}
			a.PreferredWorkerID = w.ID
			w.InitialAllocationID = a.ID
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	m.callbackMu.Lock()
	if callback != nil && m.callbackHandler != nil {
		cbID := m.callbackHandler.GenerateCallbackId()
		m.callbackHandler.SetCallback(cbID, callback)
		m.callbacks[workerID] = cbID
	}
	m.callbackMu.Unlock()
	return map[string]string{"operation_id": workerID, "worker_id": workerID}, nil
}

func reserve(a *state.Allocation, users []string, now, ttl int64) error {
	if !sameUsers(a.UserIDs, users) {
		return ErrForbidden
	}
	if a.State == "assigned" || a.State == "active" {
		return nil
	}
	if a.State != "prepared" {
		return ErrBusy
	}
	a.Sessions = make([]state.Session, len(users))
	for i, user := range a.UserIDs {
		a.Sessions[i] = state.Session{ID: id(), UserID: user, Seat: i, ExpiresAt: now + ttl}
	}
	a.State = "assigned"
	a.ExpiresAt = now + ttl
	touch(a)
	return nil
}
func (m *Manager) Join(ctx context.Context, workerID string, users []string, metadata map[string]string) (*runtime.JoinInfo, error) {
	var result *runtime.JoinInfo
	err := m.store.Update(ctx, func(s *state.State) error {
		a := s.Allocations[metadata["allocation_id"]]
		w := s.Workers[workerID]
		if a == nil || w == nil {
			return ErrNotFound
		}
		if a.WorkerID != workerID {
			return ErrForbidden
		}
		if !m.eligible(w, m.now().Unix()) {
			return ErrBusy
		}
		if err := reserve(a, users, m.now().Unix(), m.cfg.ReservationTTL); err != nil {
			return err
		}
		result = &runtime.JoinInfo{InstanceInfo: instanceInfo(w), SessionInfo: sessionInfo(a)}
		return nil
	})
	return result, err
}
func sessionInfo(a *state.Allocation) []*runtime.SessionInfo {
	out := make([]*runtime.SessionInfo, 0, len(a.Sessions))
	for _, s := range a.Sessions {
		out = append(out, &runtime.SessionInfo{UserId: s.UserID, SessionId: s.ID})
	}
	return out
}
func instanceInfo(w *state.Worker) *runtime.InstanceInfo {
	return &runtime.InstanceInfo{Id: w.ID, CreateTime: time.Unix(w.CreatedAt, 0), PlayerCount: w.PlayerCount, Status: w.State, ConnectionInfo: &runtime.ConnectionInfo{DnsName: w.Host, Port: w.Port}, Metadata: map[string]any{"provider_instance_id": w.ProviderID, "region": w.Region, "build_hash": w.BuildHash, "max_rooms": w.MaxRooms, "draining": w.Draining, "last_heartbeat": w.LastHeartbeat}}
}
func (m *Manager) Get(ctx context.Context, workerID string) (*runtime.InstanceInfo, error) {
	s, err := m.store.View(ctx)
	if err != nil {
		return nil, err
	}
	w := s.Workers[workerID]
	if w == nil || w.State == "stopped" {
		return nil, ErrNotFound
	}
	return instanceInfo(w), nil
}
func (m *Manager) List(ctx context.Context, query string, limit int, cursor string) ([]*runtime.InstanceInfo, string, error) {
	if limit < 1 || limit > 100 {
		return nil, "", fmt.Errorf("limit must be 1..100")
	}
	var filter struct {
		State  string `json:"state"`
		Region string `json:"region"`
	}
	if query != "" {
		if err := json.Unmarshal([]byte(query), &filter); err != nil {
			return nil, "", fmt.Errorf("query must be a JSON object with state/region")
		}
	}
	var after string
	if cursor != "" {
		b, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return nil, "", fmt.Errorf("invalid cursor")
		}
		after = string(b)
	}
	s, err := m.store.View(ctx)
	if err != nil {
		return nil, "", err
	}
	ids := make([]string, 0, len(s.Workers))
	for workerID, w := range s.Workers {
		if workerID > after && w.State != "stopped" && (filter.State == "" || filter.State == w.State) && (filter.Region == "" || filter.Region == w.Region) {
			ids = append(ids, workerID)
		}
	}
	sort.Strings(ids)
	var next string
	if len(ids) > limit {
		ids = ids[:limit]
		next = encoded([]byte(ids[len(ids)-1]))
	}
	out := make([]*runtime.InstanceInfo, 0, len(ids))
	for _, i := range ids {
		out = append(out, instanceInfo(s.Workers[i]))
	}
	return out, next, nil
}
func (m *Manager) Update(ctx context.Context, workerID string, playerCount int, metadata map[string]any) error {
	return m.store.Update(ctx, func(s *state.State) error {
		w := s.Workers[workerID]
		if w == nil {
			return ErrNotFound
		}
		if playerCount < 0 || playerCount > w.MaxRooms*2 {
			return fmt.Errorf("invalid player count")
		}
		if len(metadata) > 0 {
			return fmt.Errorf("use authenticated agent reports for metadata updates")
		}
		w.PlayerCount = playerCount
		return nil
	})
}
func (m *Manager) Delete(ctx context.Context, workerID string) error {
	return m.store.Update(ctx, func(s *state.State) error {
		w := s.Workers[workerID]
		if w == nil {
			return ErrNotFound
		}
		if w.State == "stopped" {
			return nil
		}
		if !drainable(w) || state.Occupied(s, workerID) > 0 || w.PlayerCount > 0 || !workFinished(w.Metrics) {
			return ErrBusy
		}
		m.beginDrain(s, w, m.now().Unix())
		return nil
	})
}
func (m *Manager) Drain(ctx context.Context, workerID string) error {
	return m.store.Update(ctx, func(s *state.State) error {
		w := s.Workers[workerID]
		if w == nil {
			return ErrNotFound
		}
		if w.State == "stopped" {
			return nil
		}
		if !drainable(w) {
			return ErrBusy
		}
		m.beginDrain(s, w, m.now().Unix())
		return nil
	})
}

func drainable(w *state.Worker) bool {
	return w.State == "ready" || w.State == "suspect" || w.State == "draining"
}

func workFinished(metrics state.Metrics) bool {
	return metrics.SimulationActive == 0 && metrics.SimulationPending == 0 && metrics.AuditActive == 0 && metrics.AuditPending == 0 && metrics.PendingResults == 0
}

func (m *Manager) beginDrain(s *state.State, w *state.Worker, now int64) {
	if w.Draining {
		return
	}
	w.Draining = true
	w.State = "draining"
	w.UpdatedAt = now
	c := &state.Command{ID: id(), Type: "drain", WorkerID: w.ID, Epoch: 1, ExpiresAt: now + 3600}
	s.Commands[c.ID] = c
}

type Endpoint struct {
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Transport string `json:"transport"`
}
type Assignment struct {
	SchemaVersion    int       `json:"schema_version"`
	AllocationID     string    `json:"allocation_id"`
	Revision         int64     `json:"revision"`
	State            string    `json:"state"`
	WorkerID         string    `json:"worker_id,omitempty"`
	BootID           string    `json:"boot_id,omitempty"`
	RoomID           string    `json:"room_id,omitempty"`
	Seat             int       `json:"seat"`
	Endpoint         *Endpoint `json:"endpoint,omitempty"`
	BuildHash        string    `json:"build_hash"`
	AdmissionToken   string    `json:"admission_token,omitempty"`
	ExpiresAt        int64     `json:"expires_at"`
	ReconnectSeconds int64     `json:"reconnect_seconds"`
	Error            string    `json:"error,omitempty"`
}

func (m *Manager) Assignment(ctx context.Context, user, allocationID string, resume bool) (*Assignment, error) {
	s, err := m.store.View(ctx)
	if err != nil {
		return nil, err
	}
	var a *state.Allocation
	if allocationID != "" {
		a = s.Allocations[allocationID]
		if a != nil && !contains(a.UserIDs, user) {
			return nil, ErrForbidden
		}
	} else {
		for _, candidate := range s.Allocations {
			if contains(candidate.UserIDs, user) && (a == nil || state.Terminal(a.State) && !state.Terminal(candidate.State) || state.Terminal(a.State) == state.Terminal(candidate.State) && candidate.CreatedAt > a.CreatedAt) {
				a = candidate
			}
		}
	}
	if a == nil {
		return nil, ErrNotFound
	}
	out := &Assignment{SchemaVersion: 1, AllocationID: a.ID, Revision: a.Revision, State: a.State, WorkerID: a.WorkerID, RoomID: a.RoomID, Seat: -1, BuildHash: m.cfg.BuildHash, ExpiresAt: a.ExpiresAt, ReconnectSeconds: m.cfg.ReconnectSeconds, Error: a.Error}
	if a.State != "assigned" && a.State != "active" {
		return out, nil
	}
	w := s.Workers[a.WorkerID]
	now := m.now().Unix()
	if w == nil || !w.Ready || w.BootID == "" || w.Host == "" || now-w.LastHeartbeat > m.cfg.HeartbeatTimeout || (w.State != "ready" && w.State != "draining") {
		return nil, ErrBusy
	}
	out.BuildHash = w.BuildHash
	for _, session := range a.Sessions {
		if session.UserID != user {
			continue
		}
		expires := session.ExpiresAt
		if resume {
			if !session.EverConnected || session.ReconnectUntil > 0 && session.ReconnectUntil <= now {
				return nil, ErrBusy
			}
			expires = now + 45
			if session.ReconnectUntil > 0 && expires > session.ReconnectUntil {
				expires = session.ReconnectUntil
			}
		} else if expires <= now {
			return nil, ErrBusy
		}
		claims := AdmissionClaims{SchemaVersion: 1, UserID: user, WorkerID: w.ID, BootID: w.BootID, RoomID: a.RoomID, AllocationID: a.ID, ReservationID: session.ID, Seat: session.Seat, Epoch: a.Epoch, ExpiresAt: expires, Nonce: id(), Resume: resume}
		token, err := sign(derive(m.cfg.SigningKey, "admission:"+w.ID), claims)
		if err != nil {
			return nil, err
		}
		out.BootID = w.BootID
		out.Seat = session.Seat
		out.Endpoint = &Endpoint{Host: w.Host, Port: w.Port, Transport: "tugboat-udp"}
		out.AdmissionToken = token
		out.ExpiresAt = expires
		return out, nil
	}
	return nil, ErrForbidden
}
