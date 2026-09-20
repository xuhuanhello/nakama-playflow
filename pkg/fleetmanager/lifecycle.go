package fleetmanager

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/xuhuanhello/nakama-playflow/internal/playflow"
	"github.com/xuhuanhello/nakama-playflow/internal/state"
)

var errLease = errors.New("control lease held by another process")

func (m *Manager) eligible(w *state.Worker, now int64) bool {
	return w.State == "ready" && w.BuildHash == m.cfg.BuildHash && w.Region == m.cfg.Region && !w.Draining && w.Ready && w.Host != "" && w.BootID != "" && now-w.LastHeartbeat <= m.cfg.HeartbeatTimeout && w.Metrics.SimulationOldestSeconds <= m.cfg.MaxQueueAgeSeconds && w.Metrics.FrameP99MS <= 100 && (w.ProviderExpiresAt == 0 || w.ProviderExpiresAt-now > m.cfg.MinLifetimeForAdmission)
}

// Tick advances durable work in short transactions and performs provider IO
// outside their locks. Ambiguous creates are reconciled, never blindly retried.
func (m *Manager) Tick(ctx context.Context) error {
	m.tickMu.Lock()
	defer m.tickMu.Unlock()
	now := m.now().Unix()
	// Standard FleetManager callbacks belong to the originating process, even
	// when a different controller currently owns provider reconciliation.
	m.deliverCallbacks(ctx)
	err := m.store.Update(ctx, func(s *state.State) error {
		if s.Leader != m.owner && s.LeaseUntil > now {
			return errLease
		}
		s.Leader = m.owner
		s.LeaseUntil = now + 30
		for _, w := range s.Workers {
			if w.State == "ready" && w.ProviderExpiresAt > 0 && w.ProviderExpiresAt-now <= m.cfg.MinLifetimeForAdmission {
				m.beginDrain(s, w, now)
			}
			if w.State == "starting" && now-w.UpdatedAt > 15 {
				w.State = "unknown"
				w.Error = "interrupted_create"
			}
			if w.State == "ready" && now-w.LastHeartbeat > m.cfg.HeartbeatTimeout {
				w.State = "suspect"
			}
			if w.LastHeartbeat > 0 && now-w.LastHeartbeat > m.cfg.LostTimeout && w.State != "stopped" && w.State != "failed" && w.State != "lost" {
				lost(s, w, "agent_lost", now)
			}
		}
		for _, a := range s.Allocations {
			if state.Terminal(a.State) || a.State == "cancelling" {
				continue
			}
			if a.PreferredWorkerID != "" {
				w := s.Workers[a.PreferredWorkerID]
				if w != nil && (w.State == "failed" || w.State == "lost" || w.State == "stopped") {
					m.cancel(s, a, "server_failed", now)
					continue
				}
			}
			if a.State == "active" {
				for _, session := range a.Sessions {
					if session.ReconnectUntil > 0 && session.ReconnectUntil <= now {
						m.cancel(s, a, "reconnect_expired", now)
						break
					}
				}
				continue
			}
			if now >= a.ExpiresAt {
				m.cancel(s, a, "allocation_expired", now)
			}
		}
		// Old terminal history is bounded, while unresolved and occupied work is retained.
		for aid, a := range s.Allocations {
			if state.Terminal(a.State) && now-a.TerminalAt > 86400 {
				delete(s.Allocations, aid)
			}
		}
		for cid, c := range s.Commands {
			if c.Done && now-c.ExpiresAt > 3600 {
				delete(s.Commands, cid)
			}
		}
		for wid, w := range s.Workers {
			if (w.State == "stopped" || w.State == "failed" && w.ProviderID == "") && now-w.UpdatedAt > 86400 {
				pending := false
				for _, a := range s.Allocations {
					if (a.WorkerID == wid || a.PreferredWorkerID == wid) && !state.Terminal(a.State) {
						pending = true
					}
				}
				if !pending {
					delete(s.Workers, wid)
					for cid, c := range s.Commands {
						if c.WorkerID == wid {
							delete(s.Commands, cid)
						}
					}
				}
			}
		}
		for _, a := range s.Allocations {
			if a.State == "prepared" {
				w := s.Workers[a.WorkerID]
				if w != nil && m.eligible(w, now) {
					if err := reserve(a, a.UserIDs, now, m.cfg.ReservationTTL); err != nil {
						return err
					}
				}
			}
		}
		waiting := make([]*state.Allocation, 0)
		for _, a := range s.Allocations {
			if a.State == "waiting_capacity" {
				waiting = append(waiting, a)
			}
		}
		sort.Slice(waiting, func(i, j int) bool {
			if (waiting[i].PreferredWorkerID != "") != (waiting[j].PreferredWorkerID != "") {
				return waiting[i].PreferredWorkerID != ""
			}
			if waiting[i].CreatedAt == waiting[j].CreatedAt {
				return waiting[i].ID < waiting[j].ID
			}
			return waiting[i].CreatedAt < waiting[j].CreatedAt
		})
		for _, a := range waiting {
			var chosen *state.Worker
			best := -1
			for _, w := range s.Workers {
				if !m.eligible(w, now) || a.PreferredWorkerID != "" && a.PreferredWorkerID != w.ID {
					continue
				}
				n := state.Occupied(s, w.ID)
				if n < w.MaxRooms && (n > best || n == best && (chosen == nil || w.ID < chosen.ID)) {
					chosen = w
					best = n
				}
			}
			if chosen == nil {
				continue
			}
			a.WorkerID = chosen.ID
			a.State = "preparing"
			a.ExpiresAt = now + m.cfg.PrepareTimeout
			touch(a)
			chosen.IdleSince = 0
			c := &state.Command{ID: id(), Type: "prepare_room", WorkerID: chosen.ID, RoomID: a.RoomID, AllocationID: a.ID, Epoch: a.Epoch, UserIDs: append([]string{}, a.UserIDs...), ExpiresAt: a.ExpiresAt}
			s.Commands[c.ID] = c
		}
		q, ready, bootCapacity := 0, 0, 0
		for _, a := range s.Allocations {
			if a.State == "waiting_capacity" && a.PreferredWorkerID == "" {
				q++
			}
		}
		for _, w := range s.Workers {
			switch w.State {
			case "requested", "starting", "launching", "bootstrapping":
				if now-w.CreatedAt < m.cfg.LaunchTimeout {
					bootCapacity += w.MaxRooms
				}
			case "ready":
				if !w.Draining {
					ready++
				}
			}
		}
		if s.CreationBlockedReason == "" && now >= s.NextCreateAt && m.liveCount(s) < m.cfg.MaxInstances && (q > bootCapacity || ready < m.cfg.MinInstances && bootCapacity == 0) {
			m.newWorker(s, now, m.cfg.MaxRooms)
			s.NextCreateAt = now + 1
		}
		for _, w := range s.Workers {
			occupied := state.Occupied(s, w.ID)
			if w.State == "ready" && !w.Draining {
				if occupied == 0 {
					if w.IdleSince == 0 {
						w.IdleSince = now
					}
					if now-w.IdleSince >= m.cfg.IdleSeconds && ready > m.cfg.MinInstances && q == 0 {
						m.beginDrain(s, w, now)
						ready--
					}
				} else {
					w.IdleSince = 0
				}
			}
			if w.State == "draining" && w.DrainAck && occupied == 0 && w.PlayerCount == 0 && workFinished(w.Metrics) && now-w.LastHeartbeat <= m.cfg.HeartbeatTimeout {
				w.State = "stopping"
				w.NextCheckAt = now
			}
		}
		return nil
	})
	if errors.Is(err, errLease) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := m.notify(ctx); err != nil {
		return err
	}
	s, err := m.store.View(ctx)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(s.Workers))
	for wid, w := range s.Workers {
		if w.State == "requested" || (now >= w.NextCheckAt && (w.State == "unknown" || w.State == "stopping" || (w.ProviderID != "" && (w.State == "launching" || w.State == "bootstrapping" || w.State == "ready" || w.State == "suspect" || w.State == "draining" || w.State == "lost")))) {
			ids = append(ids, wid)
		}
	}
	// Oldest due operation first, one per tick. A slow provider cannot make the
	// fast room loop wait behind an unbounded number of API calls.
	sort.Slice(ids, func(i, j int) bool {
		a, b := s.Workers[ids[i]], s.Workers[ids[j]]
		aDue, bDue := a.NextCheckAt, b.NextCheckAt
		if aDue == 0 {
			aDue = a.UpdatedAt
		}
		if bDue == 0 {
			bDue = b.UpdatedAt
		}
		if aDue == bDue {
			return ids[i] < ids[j]
		}
		return aDue < bDue
	})
	if len(ids) > 1 {
		ids = ids[:1]
	}
	for _, wid := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		w := s.Workers[wid]
		// A tick may span many network calls. Renew and recheck ownership before
		// each operation so an expired controller cannot keep issuing writes.
		if err = m.renewLease(ctx); err != nil {
			if errors.Is(err, errLease) {
				return nil
			}
			return err
		}
		switch w.State {
		case "requested":
			if err = m.start(ctx, w); err != nil {
				return err
			}
		case "unknown":
			if now >= w.NextCheckAt {
				if err = m.reconcileUnknown(ctx, w); err != nil {
					return err
				}
			}
		case "launching", "bootstrapping", "ready", "suspect", "draining", "lost":
			if w.ProviderID != "" && now >= w.NextCheckAt {
				if err = m.poll(ctx, w); err != nil {
					return err
				}
			}
		case "stopping":
			if now >= w.NextCheckAt {
				if err = m.stop(ctx, w); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (m *Manager) renewLease(ctx context.Context) error {
	return m.store.Update(ctx, func(s *state.State) error {
		now := m.now().Unix()
		if s.Leader != m.owner || s.LeaseUntil <= now {
			return errLease
		}
		s.LeaseUntil = now + 30
		return nil
	})
}

func stopped(s *state.State, w *state.Worker, now int64) {
	w.State = "stopped"
	w.Ready = false
	w.Error = ""
	w.UpdatedAt = now
	for _, a := range s.Allocations {
		if a.WorkerID == w.ID && !state.Terminal(a.State) {
			finish(a, "failed", "server_stopped", now)
		}
	}
	for _, command := range s.Commands {
		if command.WorkerID == w.ID {
			command.Done = true
		}
	}
}

func lost(s *state.State, w *state.Worker, reason string, now int64) {
	w.State = "lost"
	w.Ready = false
	w.Error = reason
	w.UpdatedAt = now
	for _, a := range s.Allocations {
		if a.WorkerID == w.ID && !state.Terminal(a.State) {
			finish(a, "failed", "server_lost", now)
		}
	}
	for _, command := range s.Commands {
		if command.WorkerID == w.ID {
			command.Done = true
		}
	}
}

func (m *Manager) start(ctx context.Context, w *state.Worker) error {
	now := m.now().Unix()
	if err := m.store.Update(ctx, func(s *state.State) error {
		current := s.Workers[w.ID]
		if s.Leader != m.owner || s.LeaseUntil <= now {
			return errLease
		}
		if current.State != "requested" {
			return ErrBusy
		}
		current.State = "starting"
		current.UpdatedAt = now
		return nil
	}); err != nil {
		return err
	}
	req := playflow.StartRequest{Name: "nk-" + w.ID, Region: m.cfg.Region, ComputeSize: m.cfg.ComputeSize, Version: m.cfg.ProviderVersion, AutoRestart: false,
		CustomData:           map[string]any{"fleet_owner": m.cfg.DeploymentID, "worker_id": w.ID, "build_hash": m.cfg.BuildHash},
		EnvironmentVariables: map[string]string{"FLEET_WORKER_ID": w.ID, "FLEET_BOOTSTRAP_TOKEN": encoded(derive(m.cfg.SigningKey, "bootstrap:"+w.ID)), "FLEET_ADMISSION_KEY": encoded(derive(m.cfg.SigningKey, "admission:"+w.ID)), "FLEET_CONTROL_URL": m.cfg.ControlURL, "FLEET_BUILD_HASH": m.cfg.BuildHash, "FLEET_MAX_ROOMS": fmt.Sprint(w.MaxRooms)},
	}
	call, cancel := context.WithTimeout(ctx, 5*time.Second)
	result, callErr := m.provider.Start(call, req)
	cancel()
	return m.store.Update(ctx, func(s *state.State) error {
		current := s.Workers[w.ID]
		if s.Leader != m.owner {
			return nil
		}
		current.UpdatedAt = m.now().Unix()
		if callErr != nil {
			if playflow.IsOutcomeUnknown(callErr) {
				current.State = "unknown"
				current.Error = "create_outcome_unknown"
				current.NextCheckAt = now + 1
			} else {
				current.State = "failed"
				current.Error = "provider_create_failed"
				var apiError *playflow.APIError
				if errors.As(callErr, &apiError) && (apiError.StatusCode == 400 || apiError.StatusCode == 401 || apiError.StatusCode == 403 || apiError.StatusCode == 404 || apiError.StatusCode == 422) {
					s.CreationBlockedReason = "provider_configuration_requires_review"
				}
			}
			s.NextCreateAt = now + 15
			return nil
		}
		current.ProviderID = result.InstanceID
		current.State = "launching"
		current.NextCheckAt = m.now().Unix()
		m.providerLifetime(s, current, result)
		return nil
	})
}

func (m *Manager) providerLifetime(s *state.State, w *state.Worker, instance playflow.Instance) {
	if instance.TTL <= 0 {
		return
	}
	start := w.CreatedAt
	if parsed, err := time.Parse(time.RFC3339Nano, instance.StartedAt); err == nil {
		start = parsed.Unix()
	}
	w.ProviderExpiresAt = start + int64(instance.TTL)
	if int64(instance.TTL) <= m.cfg.MinLifetimeForAdmission+m.cfg.LaunchTimeout {
		// Such an instance cannot safely accept a room under this profile. Avoid
		// an endless paid create/drain loop when the provider imposes a short TTL.
		s.CreationBlockedReason = "provider_lifetime_too_short"
		if w.State == "launching" || w.State == "bootstrapping" {
			w.State = "stopping"
			w.NextCheckAt = m.now().Unix()
		}
	}
}
func (m *Manager) endpoint(instance playflow.Instance) (string, int, error) {
	for _, p := range instance.NetworkPorts {
		if p.Name == m.cfg.PortName {
			if p.Protocol != "udp" || p.Host == "" || p.ExternalPort < 1 || p.ExternalPort > 65535 {
				return "", 0, fmt.Errorf("invalid game endpoint")
			}
			return p.Host, p.ExternalPort, nil
		}
	}
	return "", 0, fmt.Errorf("game UDP port missing")
}
func (m *Manager) owned(instance playflow.Instance, w *state.Worker) bool {
	return instance.CustomData["fleet_owner"] == m.cfg.DeploymentID && instance.CustomData["worker_id"] == w.ID
}
func (m *Manager) reconcileUnknown(ctx context.Context, w *state.Worker) error {
	call, cancel := context.WithTimeout(ctx, 5*time.Second)
	instances, err := m.provider.List(call)
	cancel()
	now := m.now().Unix()
	return m.store.Update(ctx, func(s *state.State) error {
		current := s.Workers[w.ID]
		if s.Leader != m.owner || current.State != "unknown" {
			return nil
		}
		current.NextCheckAt = now + m.cfg.ProviderPollSeconds
		if err != nil {
			return nil
		}
		var matches []playflow.Instance
		for _, i := range instances {
			if m.owned(i, w) {
				matches = append(matches, i)
			}
		}
		if len(matches) == 1 {
			current.ProviderID = matches[0].InstanceID
			current.State = "launching"
			current.Error = ""
			current.NextCheckAt = now
		} else if len(matches) > 1 {
			current.Error = "multiple_instances_require_operator_review"
		} else {
			current.Error = "create_still_unknown"
		}
		// Absence from an eventually consistent list is not proof that POST failed.
		return nil
	})
}
func (m *Manager) poll(ctx context.Context, w *state.Worker) error {
	call, cancel := context.WithTimeout(ctx, 5*time.Second)
	result, err := m.provider.Get(call, w.ProviderID)
	cancel()
	now := m.now().Unix()
	return m.store.Update(ctx, func(s *state.State) error {
		current := s.Workers[w.ID]
		if s.Leader != m.owner || current.ProviderID != w.ProviderID {
			return nil
		}
		current.NextCheckAt = now + m.cfg.ProviderPollSeconds
		if err != nil {
			var apiError *playflow.APIError
			if errors.As(err, &apiError) && apiError.StatusCode == 404 {
				stopped(s, current, now)
				return nil
			}
			current.Error = "provider_query_failed"
			return nil
		}
		if !m.owned(result, w) {
			lost(s, current, "provider_ownership_mismatch", now)
			return nil
		}
		m.providerLifetime(s, current, result)
		if current.State == "stopping" {
			return nil
		}
		if result.Status == "stopped" {
			stopped(s, current, now)
			return nil
		}
		if current.State == "lost" {
			return nil
		}
		if now-current.CreatedAt >= m.cfg.LaunchTimeout && (current.State == "launching" || current.State == "bootstrapping") {
			current.State = "stopping"
			current.Error = "launch_timeout"
			current.NextCheckAt = now
			return nil
		}
		if result.Status == "running" {
			host, port, portErr := m.endpoint(result)
			if portErr != nil {
				current.Error = "invalid_provider_endpoint"
				return nil
			}
			current.Host = host
			current.Port = port
			if current.Ready && current.BootID != "" && now-current.LastHeartbeat <= m.cfg.HeartbeatTimeout {
				if current.Draining {
					current.State = "draining"
				} else {
					current.State = "ready"
				}
				current.Error = ""
			} else if current.State == "launching" || current.State == "bootstrapping" {
				current.State = "bootstrapping"
			} else if !current.Draining {
				current.State = "suspect"
			}
		}
		return nil
	})
}
func (m *Manager) stop(ctx context.Context, w *state.Worker) error {
	// Recheck after the snapshot: a fresh heartbeat may reveal pending work.
	if err := m.store.Update(ctx, func(s *state.State) error {
		current := s.Workers[w.ID]
		if s.Leader != m.owner || s.LeaseUntil <= m.now().Unix() {
			return errLease
		}
		if current == nil || current.State != "stopping" || current.ProviderID != w.ProviderID {
			return ErrBusy
		}
		if current.Draining && (!current.DrainAck || state.Occupied(s, w.ID) > 0 || current.PlayerCount > 0 || !workFinished(current.Metrics) || m.now().Unix()-current.LastHeartbeat > m.cfg.HeartbeatTimeout) {
			return ErrBusy
		}
		return nil
	}); err != nil {
		return err
	}
	call, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := m.provider.Stop(call, w.ProviderID)
	cancel()
	now := m.now().Unix()
	if err != nil {
		return m.store.Update(ctx, func(s *state.State) error {
			if s.Leader == m.owner {
				current := s.Workers[w.ID]
				current.Error = "provider_stop_failed"
				current.NextCheckAt = now + m.cfg.ProviderPollSeconds
			}
			return nil
		})
	}
	// A successful DELETE is followed by a read before resource accounting closes.
	call, cancel = context.WithTimeout(ctx, 5*time.Second)
	result, getErr := m.provider.Get(call, w.ProviderID)
	cancel()
	return m.store.Update(ctx, func(s *state.State) error {
		if s.Leader != m.owner {
			return nil
		}
		current := s.Workers[w.ID]
		current.NextCheckAt = now + m.cfg.ProviderPollSeconds
		var apiError *playflow.APIError
		if getErr == nil && result.Status == "stopped" && m.owned(result, w) || errors.As(getErr, &apiError) && apiError.StatusCode == 404 {
			stopped(s, current, now)
		} else {
			current.Error = "awaiting_stop_confirmation"
		}
		return nil
	})
}
func (m *Manager) deliverCallbacks(ctx context.Context) {
	m.callbackMu.Lock()
	handler := m.callbackHandler
	m.callbackMu.Unlock()
	if handler == nil {
		return
	}
	s, err := m.store.View(ctx)
	if err != nil {
		return
	}
	type delivery struct {
		id     string
		status runtime.FmCreateStatus
		w      *state.Worker
		a      *state.Allocation
		err    error
	}
	var ready []delivery
	m.callbackMu.Lock()
	for wid, cbID := range m.callbacks {
		w := s.Workers[wid]
		if w == nil {
			continue
		}
		var a *state.Allocation
		if w.InitialAllocationID != "" {
			a = s.Allocations[w.InitialAllocationID]
		}
		if w.State == "ready" && (a == nil || a.State == "assigned" || a.State == "active") {
			ready = append(ready, delivery{id: cbID, status: runtime.CreateSuccess, w: w, a: a})
			delete(m.callbacks, wid)
		} else if w.State == "failed" || w.State == "stopped" || w.State == "lost" || m.now().Unix()-w.CreatedAt > m.cfg.LaunchTimeout {
			ready = append(ready, delivery{id: cbID, status: runtime.CreateTimeout, err: fmt.Errorf("instance creation did not become ready")})
			delete(m.callbacks, wid)
		}
	}
	m.callbackMu.Unlock()
	for _, d := range ready {
		if d.w == nil {
			handler.InvokeCallback(d.id, d.status, nil, nil, nil, d.err)
		} else {
			var sessions []*runtime.SessionInfo
			if d.a != nil {
				sessions = sessionInfo(d.a)
			}
			handler.InvokeCallback(d.id, d.status, instanceInfo(d.w), sessions, nil, nil)
		}
	}
}
func (m *Manager) notify(ctx context.Context) error {
	m.callbackMu.Lock()
	nk := m.nk
	m.callbackMu.Unlock()
	if nk == nil {
		return nil
	}
	s, err := m.store.View(ctx)
	if err != nil {
		return err
	}
	for _, a := range s.Allocations {
		if a.NotifiedRevision >= a.Revision {
			continue
		}
		success := true
		for _, user := range a.UserIDs {
			if err = nk.NotificationSend(ctx, user, "fleet_assignment", map[string]any{"allocation_id": a.ID, "revision": a.Revision, "state": a.State}, 1001, "", false); err != nil {
				success = false
			}
		}
		if success {
			if err = m.store.Update(ctx, func(current *state.State) error {
				live := current.Allocations[a.ID]
				if live != nil && live.Revision == a.Revision {
					live.NotifiedRevision = a.Revision
				}
				return nil
			}); err != nil {
				return err
			}
		}
	}
	return nil
}
