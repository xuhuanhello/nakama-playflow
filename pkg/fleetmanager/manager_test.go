package fleetmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/xuhuanhello/nakama-playflow/internal/playflow"
	"github.com/xuhuanhello/nakama-playflow/internal/state"
)

type fakeProvider struct {
	instances     map[string]playflow.Instance
	starts, stops int
	ambiguous     bool
	stopErr       error
	ttl           int
}

type testCallbacks struct {
	fn    runtime.FmCreateCallbackFn
	calls int
}

func (c *testCallbacks) GenerateCallbackId() string                          { return "callback" }
func (c *testCallbacks) SetCallback(_ string, fn runtime.FmCreateCallbackFn) { c.fn = fn }
func (c *testCallbacks) InvokeCallback(_ string, status runtime.FmCreateStatus, i *runtime.InstanceInfo, sessions []*runtime.SessionInfo, metadata map[string]any, err error) {
	c.calls++
	c.fn(status, i, sessions, metadata, err)
}

func TestPreferredSeatsAndCallbackOnNonLeader(t *testing.T) {
	f := setup(t)
	f.allocate("older")
	f.clock.Add(1)
	creator, err := New(f.m.cfg, f.store, f.provider)
	if err != nil {
		t.Fatal(err)
	}
	creator.now = f.m.now
	handler := &testCallbacks{}
	_ = creator.Init(nil, handler)
	result, err := creator.Create(context.Background(), 2, []string{"preferred-a", "preferred-b"}, nil, nil, func(status runtime.FmCreateStatus, i *runtime.InstanceInfo, sessions []*runtime.SessionInfo, _ map[string]any, err error) {
		if status != runtime.CreateSuccess || err != nil || i == nil || len(sessions) != 2 {
			t.Errorf("bad callback: status %v", status)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	wid := result["worker_id"]
	f.tick()
	f.bootstrap(wid)
	f.heartbeat(wid, nil, nil, state.Metrics{})
	f.tick()
	f.tick()
	s := f.snapshot()
	aid := s.Workers[wid].InitialAllocationID
	if s.Allocations[aid].WorkerID != wid || s.Allocations[aid].State != "preparing" {
		t.Fatal("generic allocation stole promised initial seats")
	}
	response := f.heartbeat(wid, nil, nil, state.Metrics{})
	var results []CommandResult
	for _, cmd := range response.Commands {
		results = append(results, CommandResult{CommandID: cmd.ID, Success: true})
	}
	f.heartbeat(wid, nil, results, state.Metrics{})
	if err = creator.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if handler.calls != 1 {
		t.Fatal("non-leader origin did not deliver its own callback")
	}
	_ = creator.Tick(context.Background())
	if handler.calls != 1 {
		t.Fatal("callback delivered twice")
	}
}

func TestOwnershipMismatchReleasesUsersButNeverDeletesForeignInstance(t *testing.T) {
	f := setup(t)
	wid := f.readyWorker()
	aid := f.assign(wid, "owned")
	a := f.snapshot().Allocations[aid]
	f.heartbeat(wid, []RoomReport{{RoomID: a.RoomID, State: "playing", UserIDs: a.UserIDs}}, nil, state.Metrics{})
	pid := f.snapshot().Workers[wid].ProviderID
	i := f.provider.instances[pid]
	i.CustomData = map[string]any{"fleet_owner": "somebody-else"}
	f.provider.instances[pid] = i
	f.update(func(s *state.State) { s.Workers[wid].NextCheckAt = f.clock.Load() })
	f.tick()
	s := f.snapshot()
	if s.Workers[wid].State != "lost" || s.Allocations[aid].State != "failed" || f.provider.stops != 0 {
		t.Fatal("ownership mismatch must fence users and isolate foreign resource")
	}
}

func (p *fakeProvider) Start(_ context.Context, req playflow.StartRequest) (playflow.Instance, error) {
	p.starts++
	i := playflow.Instance{InstanceID: fmt.Sprintf("provider-%d", p.starts), Status: "running", CustomData: req.CustomData, NetworkPorts: []playflow.Port{{Name: "game", Host: "127.0.0.1", Protocol: "udp", ExternalPort: 30000 + p.starts}}}
	i.TTL = p.ttl
	p.instances[i.InstanceID] = i
	if p.ambiguous {
		return playflow.Instance{}, &playflow.APIError{StatusCode: 503, OutcomeUnknown: true}
	}
	return i, nil
}
func (p *fakeProvider) Get(_ context.Context, id string) (playflow.Instance, error) {
	i, ok := p.instances[id]
	if !ok {
		return i, &playflow.APIError{StatusCode: 404}
	}
	return i, nil
}
func (p *fakeProvider) List(context.Context) ([]playflow.Instance, error) {
	out := []playflow.Instance{}
	for _, i := range p.instances {
		out = append(out, i)
	}
	return out, nil
}
func (p *fakeProvider) Stop(_ context.Context, id string) error {
	p.stops++
	if p.stopErr != nil {
		return p.stopErr
	}
	delete(p.instances, id)
	return nil
}

type fixture struct {
	t         *testing.T
	m         *Manager
	store     *state.Memory
	provider  *fakeProvider
	clock     atomic.Int64
	sequences map[string]uint64
}

func testConfig() Config {
	return Config{DeploymentID: "test", BuildHash: "test-build", Region: "local", ComputeSize: "small", PortName: "game", ProviderVersion: 1, MaxRooms: 2, MaxInstances: 3, PlayFlowAPIKey: "test", ControlURL: "https://control.invalid", AdminToken: "test-admin-token-at-least-32-characters", SigningKey: bytes.Repeat([]byte{1}, 32), Mode: "production", Interval: time.Second, LaunchTimeout: 120, AllocationTimeout: 180, PrepareTimeout: 20, ReservationTTL: 60, ReconnectSeconds: 90, HeartbeatTimeout: 8, LostTimeout: 60, IdleSeconds: 600, ProviderPollSeconds: 10, MinLifetimeForAdmission: 1800, MaxQueueAgeSeconds: 2}
}
func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, store: state.NewMemory(), provider: &fakeProvider{instances: map[string]playflow.Instance{}}, sequences: map[string]uint64{}}
	f.clock.Store(1700000000)
	var err error
	f.m, err = New(testConfig(), f.store, f.provider)
	if err != nil {
		t.Fatal(err)
	}
	f.m.now = func() time.Time { return time.Unix(f.clock.Load(), 0) }
	return f
}
func (f *fixture) update(fn func(*state.State)) {
	f.t.Helper()
	if err := f.store.Update(context.Background(), func(s *state.State) error { fn(s); return nil }); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) snapshot() *state.State {
	f.t.Helper()
	s, err := f.store.View(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}
func (f *fixture) tick() {
	f.t.Helper()
	if err := f.m.Tick(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) request(path, token string, body any) (int, []byte) {
	f.t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	f.m.HTTPHandler().ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}
func (f *fixture) bootstrap(worker string) string {
	f.t.Helper()
	status, body := f.request("/fleet/v1/agent/bootstrap", "", BootstrapRequest{WorkerID: worker, BootID: "boot-" + worker, BuildHash: f.m.cfg.BuildHash, BootstrapToken: encoded(derive(f.m.cfg.SigningKey, "bootstrap:"+worker))})
	if status != 200 {
		f.t.Fatalf("bootstrap %d %s", status, body)
	}
	var out struct {
		Token string `json:"agent_token"`
	}
	_ = json.Unmarshal(body, &out)
	return out.Token
}
func (f *fixture) heartbeat(worker string, rooms []RoomReport, results []CommandResult, metrics state.Metrics) HeartbeatResponse {
	f.t.Helper()
	f.sequences[worker]++
	status, body := f.request("/fleet/v1/agent/heartbeat", encoded(derive(f.m.cfg.SigningKey, "agent:"+worker+":boot-"+worker)), HeartbeatRequest{WorkerID: worker, BootID: "boot-" + worker, Sequence: f.sequences[worker], Ready: true, Rooms: rooms, CommandResults: results, Metrics: metrics})
	if status != 200 {
		f.t.Fatalf("heartbeat %d %s", status, body)
	}
	var out HeartbeatResponse
	if err := json.Unmarshal(body, &out); err != nil {
		f.t.Fatal(err)
	}
	return out
}
func (f *fixture) readyWorker() string {
	f.t.Helper()
	result, err := f.m.Create(context.Background(), 4, nil, nil, nil, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	wid := result["worker_id"]
	f.tick()
	f.bootstrap(wid)
	f.heartbeat(wid, nil, nil, state.Metrics{})
	f.tick()
	if got := f.snapshot().Workers[wid].State; got != "ready" {
		f.t.Fatalf("worker %s", got)
	}
	return wid
}
func (f *fixture) allocate(key string) string {
	f.t.Helper()
	aid, err := f.m.Allocate(context.Background(), []string{key + "-a", key + "-b"}, key)
	if err != nil {
		f.t.Fatal(err)
	}
	return aid
}
func (f *fixture) assign(worker, key string) string {
	f.t.Helper()
	aid := f.allocate(key)
	f.tick()
	response := f.heartbeat(worker, nil, nil, state.Metrics{})
	var results []CommandResult
	for _, c := range response.Commands {
		if c.AllocationID == aid {
			results = append(results, CommandResult{CommandID: c.ID, Success: true})
		}
	}
	if len(results) != 1 {
		f.t.Fatalf("missing prepare for %s", aid)
	}
	f.heartbeat(worker, nil, results, state.Metrics{})
	if f.snapshot().Allocations[aid].State != "assigned" {
		f.t.Fatal("prepare ACK must atomically reserve both players")
	}
	return aid
}

func TestProviderRunningRequiresAgentReady(t *testing.T) {
	f := setup(t)
	aid := f.allocate("one")
	f.tick()
	f.tick()
	s := f.snapshot()
	if s.Allocations[aid].State != "waiting_capacity" {
		t.Fatal("provider running is insufficient")
	}
	var wid string
	for id := range s.Workers {
		wid = id
	}
	f.bootstrap(wid)
	f.heartbeat(wid, nil, nil, state.Metrics{})
	f.tick()
	if f.snapshot().Allocations[aid].State != "preparing" {
		t.Fatal("ready worker should receive room preparation")
	}
}

func TestRoomPackingAndAtomicSeats(t *testing.T) {
	f := setup(t)
	wid := f.readyWorker()
	a := f.assign(wid, "first")
	b := f.assign(wid, "second")
	third := f.allocate("third")
	f.tick()
	s := f.snapshot()
	if s.Allocations[a].WorkerID != wid || s.Allocations[b].WorkerID != wid || s.Allocations[a].RoomID == s.Allocations[b].RoomID {
		t.Fatal("two rooms must share one physical worker")
	}
	if state.Occupied(s, wid) != 2 || s.Allocations[third].WorkerID != "" || f.provider.starts != 2 {
		t.Fatal("full room capacity should start exactly one additional instance")
	}
	if len(s.Allocations[a].Sessions) != 2 {
		t.Fatal("partial reservation")
	}
	if _, err := f.m.Join(context.Background(), wid, []string{"first-a"}, map[string]string{"allocation_id": a}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("half join accepted: %v", err)
	}
	for _, user := range s.Allocations[a].UserIDs {
		out, err := f.m.Assignment(context.Background(), user, a, false)
		if err != nil {
			t.Fatal(err)
		}
		claims, err := VerifyAdmission(derive(f.m.cfg.SigningKey, "admission:"+wid), out.AdmissionToken, f.clock.Load())
		if err != nil || claims.UserID != user || claims.RoomID != s.Allocations[a].RoomID {
			t.Fatal("invalid signed assignment")
		}
	}
}

func TestConcurrentAllocationDedupAndRollback(t *testing.T) {
	f := setup(t)
	var wg sync.WaitGroup
	var successes atomic.Int32
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := f.m.Allocate(context.Background(), []string{"same-user", fmt.Sprintf("other-%d", i)}, fmt.Sprintf("request-%d", i))
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrBusy) {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if successes.Load() != 1 || len(f.snapshot().Allocations) != 1 {
		t.Fatal("concurrent admission allocated one user twice")
	}
	var original *state.Allocation
	for _, a := range f.snapshot().Allocations {
		original = a
	}
	again, err := f.m.Allocate(context.Background(), original.UserIDs, original.RequestKey)
	if err != nil || again != original.ID {
		t.Fatal("idempotency failed")
	}
	if _, err = f.m.Create(context.Background(), 4, original.UserIDs, nil, nil, nil); !errors.Is(err, ErrBusy) {
		t.Fatal("busy user Create should rollback")
	}
	if len(f.snapshot().Workers) != 0 {
		t.Fatal("transaction leaked a worker")
	}
}

func TestCancelWaitsForCleanupAndRetriesNewCommand(t *testing.T) {
	f := setup(t)
	wid := f.readyWorker()
	aid := f.allocate("cancel")
	f.tick()
	before := f.heartbeat(wid, nil, nil, state.Metrics{})
	prepare := before.Commands[0]
	if err := f.m.Cancel(context.Background(), "cancel-a", aid); err != nil {
		t.Fatal(err)
	}
	if state.Occupied(f.snapshot(), wid) != 1 {
		t.Fatal("cancel prematurely frees capacity")
	}
	response := f.heartbeat(wid, nil, nil, state.Metrics{})
	if len(response.Commands) != 1 || response.Commands[0].Type != "cancel_room" {
		t.Fatal("cancel must supersede preparation")
	}
	cancel := response.Commands[0]
	f.heartbeat(wid, nil, []CommandResult{{CommandID: cancel.ID, Success: false}}, state.Metrics{})
	f.clock.Add(4)
	response = f.heartbeat(wid, nil, nil, state.Metrics{})
	if len(response.Commands) != 1 || response.Commands[0].ID == cancel.ID {
		t.Fatal("failed cleanup must get a new execution ID")
	}
	f.heartbeat(wid, nil, []CommandResult{{CommandID: response.Commands[0].ID, Success: true}, {CommandID: prepare.ID, Success: true}}, state.Metrics{})
	s := f.snapshot()
	if s.Allocations[aid].State != "cancelled" || state.Occupied(s, wid) != 0 {
		t.Fatal("late prepare ACK resurrected cancelled room")
	}
}

func TestMatureWorkerControlLossNeverTriggersStartupStop(t *testing.T) {
	f := setup(t)
	wid := f.readyWorker()
	aid := f.assign(wid, "live")
	a := f.snapshot().Allocations[aid]
	f.heartbeat(wid, []RoomReport{{RoomID: a.RoomID, State: "playing", UserIDs: a.UserIDs}}, nil, state.Metrics{})
	f.update(func(s *state.State) { s.Workers[wid].CreatedAt = f.clock.Load() - 1000 })
	f.clock.Add(9)
	f.tick()
	f.clock.Add(11)
	f.tick()
	s := f.snapshot()
	if s.Workers[wid].State != "suspect" || s.Allocations[aid].State != "active" || f.provider.stops != 0 {
		t.Fatal("temporary control loss stopped a live process")
	}
	f.clock.Add(41)
	f.tick()
	s = f.snapshot()
	if s.Workers[wid].State != "lost" || s.Allocations[aid].State != "failed" || f.provider.stops != 0 {
		t.Fatal("lost process must remain quarantined, counted, and not force-deleted")
	}
	if err := f.m.Drain(context.Background(), wid); !errors.Is(err, ErrBusy) {
		t.Fatal("drain must not resurrect quarantined worker")
	}
}

func TestDrainWaitsForRoomsAndAllWorkThenConfirms404(t *testing.T) {
	f := setup(t)
	wid := f.readyWorker()
	aid := f.assign(wid, "drain")
	a := f.snapshot().Allocations[aid]
	if err := f.m.Delete(context.Background(), wid); !errors.Is(err, ErrBusy) {
		t.Fatal("delete accepted occupied instance")
	}
	if err := f.m.Drain(context.Background(), wid); err != nil {
		t.Fatal(err)
	}
	response := f.heartbeat(wid, nil, nil, state.Metrics{})
	var ack []CommandResult
	for _, c := range response.Commands {
		if c.Type == "drain" {
			ack = append(ack, CommandResult{CommandID: c.ID, Success: true})
		}
	}
	f.heartbeat(wid, []RoomReport{{RoomID: a.RoomID, State: "closed"}}, ack, state.Metrics{PendingResults: 1})
	f.tick()
	if f.provider.stops != 0 {
		t.Fatal("deleted with pending durable results")
	}
	f.heartbeat(wid, nil, nil, state.Metrics{SimulationActive: 1})
	f.tick()
	if f.provider.stops != 0 {
		t.Fatal("deleted with running simulation")
	}
	f.heartbeat(wid, nil, nil, state.Metrics{})
	f.tick()
	if f.provider.stops != 1 || f.snapshot().Workers[wid].State != "stopped" {
		t.Fatal("delete + confirmed absence should release provider capacity")
	}
}

func TestAmbiguousCreateIsAdoptedWithoutDuplicatePost(t *testing.T) {
	f := setup(t)
	f.provider.ambiguous = true
	f.allocate("unknown")
	f.tick()
	if f.provider.starts != 1 {
		t.Fatal("start missing")
	}
	var wid string
	for id, w := range f.snapshot().Workers {
		wid = id
		if w.State != "unknown" {
			t.Fatal("ambiguous start not quarantined")
		}
	}
	f.clock.Add(2)
	f.tick()
	s := f.snapshot()
	if f.provider.starts != 1 || s.Workers[wid].ProviderID == "" || s.Workers[wid].State != "launching" {
		t.Fatal("reconciliation duplicated provider creation")
	}
}

func TestSessionResumeBoundToReconnectWindow(t *testing.T) {
	f := setup(t)
	wid := f.readyWorker()
	aid := f.assign(wid, "resume")
	a := f.snapshot().Allocations[aid]
	f.heartbeat(wid, []RoomReport{{RoomID: a.RoomID, State: "playing", UserIDs: a.UserIDs}}, nil, state.Metrics{})
	f.heartbeat(wid, []RoomReport{{RoomID: a.RoomID, State: "playing", UserIDs: []string{"resume-b"}}}, nil, state.Metrics{})
	f.clock.Add(85)
	f.heartbeat(wid, []RoomReport{{RoomID: a.RoomID, State: "playing", UserIDs: []string{"resume-b"}}}, nil, state.Metrics{})
	out, err := f.m.Assignment(context.Background(), "resume-a", aid, true)
	if err != nil {
		t.Fatal(err)
	}
	if out.ExpiresAt != f.clock.Load()+5 {
		t.Fatal("resume token outlives reconnect lease")
	}
	f.clock.Add(5)
	if _, err = f.m.Assignment(context.Background(), "resume-a", aid, true); !errors.Is(err, ErrBusy) {
		t.Fatal("resume accepted after deadline")
	}
	if _, err = f.m.Assignment(context.Background(), "stranger", aid, true); !errors.Is(err, ErrForbidden) {
		t.Fatal("foreign user obtained token")
	}
}

func TestHeartbeatReplayAndBootstrapFencing(t *testing.T) {
	f := setup(t)
	wid := f.readyWorker()
	before := f.snapshot().Workers[wid].LastHeartbeat
	f.clock.Add(5)
	status, _ := f.request("/fleet/v1/agent/heartbeat", encoded(derive(f.m.cfg.SigningKey, "agent:"+wid+":boot-"+wid)), HeartbeatRequest{WorkerID: wid, BootID: "boot-" + wid, Sequence: f.sequences[wid], Ready: true})
	if status != 200 || f.snapshot().Workers[wid].LastHeartbeat != before {
		t.Fatal("replayed sequence refreshed liveness")
	}
	status, _ = f.request("/fleet/v1/agent/bootstrap", "", BootstrapRequest{WorkerID: wid, BootID: "different-process", BuildHash: f.m.cfg.BuildHash, BootstrapToken: encoded(derive(f.m.cfg.SigningKey, "bootstrap:"+wid))})
	if status != 403 {
		t.Fatal("new process reused old reservations")
	}
	status, _ = f.request("/fleet/v1/agent/heartbeat", "wrong", HeartbeatRequest{WorkerID: wid, BootID: "boot-" + wid, Sequence: 100})
	if status != 403 {
		t.Fatal("unauthenticated heartbeat accepted")
	}
}

func TestControllerLeaseAndProfileFencing(t *testing.T) {
	f := setup(t)
	f.allocate("lease")
	other, err := New(f.m.cfg, f.store, f.provider)
	if err != nil {
		t.Fatal(err)
	}
	other.now = f.m.now
	f.tick()
	if err = other.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.snapshot().Leader != f.m.owner || f.provider.starts != 1 {
		t.Fatal("two controllers claimed same work")
	}
	f.clock.Add(31)
	if err = other.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = f.m.renewLease(context.Background()); !errors.Is(err, errLease) {
		t.Fatal("expired controller resumed IO")
	}
	cfg := f.m.cfg
	cfg.BuildHash = "other-build"
	if _, err = New(cfg, f.store, f.provider); err == nil {
		t.Fatal("profile drift admitted incompatible binaries")
	}
}

func TestAdmissionTokenTamperAndExpiry(t *testing.T) {
	f := setup(t)
	wid := f.readyWorker()
	aid := f.assign(wid, "token")
	out, err := f.m.Assignment(context.Background(), "token-a", aid, false)
	if err != nil {
		t.Fatal(err)
	}
	key := derive(f.m.cfg.SigningKey, "admission:"+wid)
	if _, err = VerifyAdmission(key, out.AdmissionToken+"x", f.clock.Load()); err == nil {
		t.Fatal("tampered token accepted")
	}
	if _, err = VerifyAdmission(key, out.AdmissionToken, f.clock.Load()+60); err == nil {
		t.Fatal("expired token accepted")
	}
	if _, err = VerifyAdmission(bytes.Repeat([]byte{3}, 32), out.AdmissionToken, f.clock.Load()); err == nil {
		t.Fatal("wrong worker key accepted")
	}
}

func TestProviderLifetimeTriggersEarlyDrainWithoutKillingActiveRoom(t *testing.T) {
	f := setup(t)
	f.provider.ttl = 3600
	wid := f.readyWorker()
	aid := f.assign(wid, "ttl")
	a := f.snapshot().Allocations[aid]
	f.heartbeat(wid, []RoomReport{{RoomID: a.RoomID, State: "playing", UserIDs: a.UserIDs}}, nil, state.Metrics{})
	f.clock.Add(1801)
	f.heartbeat(wid, []RoomReport{{RoomID: a.RoomID, State: "playing", UserIDs: a.UserIDs}}, nil, state.Metrics{})
	f.tick()
	s := f.snapshot()
	if s.Workers[wid].State != "draining" || s.Allocations[aid].State != "active" || f.provider.stops != 0 {
		t.Fatal("TTL margin must begin graceful drain without stopping active game")
	}
	if f.m.eligible(s.Workers[wid], f.clock.Load()) {
		t.Fatal("near-expiry instance still accepts rooms")
	}
}

func TestTooShortProviderLifetimeBlocksCreateChurn(t *testing.T) {
	f := setup(t)
	f.provider.ttl = 60
	f.allocate("short-ttl")
	f.tick()
	f.tick()
	f.clock.Add(20)
	f.tick()
	s := f.snapshot()
	if s.CreationBlockedReason != "provider_lifetime_too_short" || f.provider.starts != 1 || f.provider.stops != 1 {
		t.Fatal("short forced TTL repeatedly creates unusable instances")
	}
}
