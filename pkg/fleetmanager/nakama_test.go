package fleetmanager

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/heroiclabs/nakama-common/rtapi"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/xuhuanhello/nakama-playflow/internal/state"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type beforeRealtimeHook = func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *rtapi.Envelope) (*rtapi.Envelope, error)
type playerRPCHook = func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, string) (string, error)
type matchedHook = func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, []runtime.MatchmakerEntry) (string, error)

type playerHookInitializer struct {
	runtime.Initializer
	beforeName string
	before     beforeRealtimeHook
	matched    matchedHook
	rpcs       map[string]playerRPCHook
	beforeErr  error
}

func (i *playerHookInitializer) RegisterBeforeRt(name string, fn beforeRealtimeHook) error {
	i.beforeName, i.before = name, fn
	return i.beforeErr
}
func (i *playerHookInitializer) RegisterMatchmakerMatched(fn matchedHook) error {
	i.matched = fn
	return nil
}
func (i *playerHookInitializer) RegisterRpc(name string, fn playerRPCHook) error {
	if i.rpcs == nil {
		i.rpcs = make(map[string]playerRPCHook)
	}
	i.rpcs[name] = fn
	return nil
}

func playerHooks(t *testing.T, m *Manager) *playerHookInitializer {
	t.Helper()
	initializer := &playerHookInitializer{}
	if err := registerPlayerHooks(initializer, m); err != nil {
		t.Fatal(err)
	}
	if initializer.beforeName != "MatchmakerAdd" || initializer.before == nil || initializer.matched == nil {
		t.Fatal("queue-admission and matched hooks must both be registered")
	}
	if len(initializer.rpcs) != 3 || initializer.rpcs["fleet_assignment_get_v1"] == nil || initializer.rpcs["fleet_resume_v1"] == nil || initializer.rpcs["fleet_assignment_cancel_v1"] == nil {
		t.Fatal("the existing player RPC surface must remain unchanged")
	}
	return initializer
}

func assertPlayerError(t *testing.T, err error, code int, message string) {
	t.Helper()
	var result *runtime.Error
	if !errors.As(err, &result) || result.Code != code || result.Message != message {
		t.Fatalf("got %v; want Nakama runtime error %d %q", err, code, message)
	}
}

func matchmakerEnvelope(buildHash, region string) *rtapi.Envelope {
	return &rtapi.Envelope{Cid: "match-request-17", Message: &rtapi.Envelope_MatchmakerAdd{MatchmakerAdd: &rtapi.MatchmakerAdd{
		MinCount: 2, MaxCount: 2, CountMultiple: wrapperspb.Int32(2),
		Query:             `+properties.group:"group-42" +properties.mode:ranked`,
		StringProperties:  map[string]string{"build_hash": buildHash, "region": region, "group": "group-42", "mode": "ranked"},
		NumericProperties: map[string]float64{"skill": 1234, "group_size": 2},
	}}}
}

func TestMatchmakerAddRejectsIncompatibleProfileBeforeAdmission(t *testing.T) {
	f := setup(t)
	hooks := playerHooks(t, f.m)
	for _, test := range []struct {
		name, buildHash, region, message string
	}{
		{"old_build", "old-build", f.m.cfg.Region, "fleet_build_mismatch"},
		{"wrong_region", f.m.cfg.BuildHash, "other-region", "fleet_region_mismatch"},
		{"both_wrong_build_first", "old-build", "other-region", "fleet_build_mismatch"},
		{"missing_build", "", f.m.cfg.Region, "fleet_build_mismatch"},
		{"missing_region", f.m.cfg.BuildHash, "", "fleet_region_mismatch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := matchmakerEnvelope(test.buildHash, test.region)
			if test.buildHash == "" {
				delete(request.GetMatchmakerAdd().StringProperties, "build_hash")
			}
			if test.region == "" {
				delete(request.GetMatchmakerAdd().StringProperties, "region")
			}
			before := f.snapshot()
			response, err := hooks.before(context.Background(), nil, nil, nil, request)
			assertPlayerError(t, err, 9, test.message)
			if response != nil {
				t.Fatal("rejected matchmaker request must not continue to admission")
			}
			if !reflect.DeepEqual(before, f.snapshot()) || f.provider.starts != 0 {
				t.Fatal("rejected queue admission mutated fleet state or started a server")
			}
		})
	}
}

func TestMatchmakerAddPreservesGroupQueryAndEnvelope(t *testing.T) {
	f := setup(t)
	hooks := playerHooks(t, f.m)
	request := matchmakerEnvelope(f.m.cfg.BuildHash, f.m.cfg.Region)
	before := proto.Clone(request)
	response, err := hooks.before(context.Background(), nil, nil, nil, request)
	if err != nil || response != request || !proto.Equal(before, response) {
		t.Fatalf("compatible request must pass through unchanged: %v", err)
	}
}

func TestMatchmakerAddInvalidEnvelopeAndRegistrationFailure(t *testing.T) {
	f := setup(t)
	hooks := playerHooks(t, f.m)
	for _, request := range []*rtapi.Envelope{nil, {Cid: "missing-matchmaker-add"}} {
		response, err := hooks.before(context.Background(), nil, nil, nil, request)
		assertPlayerError(t, err, 3, "invalid matchmaker request")
		if response != nil {
			t.Fatal("invalid envelope passed to queue admission")
		}
	}
	errHook := errors.New("before-hook registration failed")
	failed := &playerHookInitializer{beforeErr: errHook}
	if err := registerPlayerHooks(failed, f.m); !errors.Is(err, errHook) {
		t.Fatalf("hook registration failure must abort setup: %v", err)
	}
	if failed.matched != nil || len(failed.rpcs) != 0 {
		t.Fatal("registration continued after queue-validation hook failed")
	}
}

type profileMatchmakerEntry struct {
	runtime.MatchmakerEntry
	user, ticket string
	properties   map[string]any
}

type profilePresence struct {
	runtime.Presence
	user string
}

func (p profilePresence) GetUserId() string { return p.user }
func (e profileMatchmakerEntry) GetPresence() runtime.Presence {
	return profilePresence{user: e.user}
}
func (e profileMatchmakerEntry) GetTicket() string             { return e.ticket }
func (e profileMatchmakerEntry) GetProperties() map[string]any { return e.properties }

func TestMatchedHookRetainsFinalProfileValidation(t *testing.T) {
	for _, property := range []string{"build_hash", "region"} {
		t.Run(property, func(t *testing.T) {
			f := setup(t)
			hooks := playerHooks(t, f.m)
			properties := map[string]any{"build_hash": f.m.cfg.BuildHash, "region": f.m.cfg.Region, "group": "group-42"}
			incompatible := map[string]any{"build_hash": f.m.cfg.BuildHash, "region": f.m.cfg.Region}
			incompatible[property] = "incompatible"
			entries := []runtime.MatchmakerEntry{
				profileMatchmakerEntry{user: "player-1", ticket: "ticket-1", properties: properties},
				profileMatchmakerEntry{user: "player-2", ticket: "ticket-2", properties: incompatible},
			}
			message := "fleet_region_mismatch"
			if property == "build_hash" {
				message = "fleet_build_mismatch"
			}
			_, err := hooks.matched(context.Background(), nil, nil, nil, entries)
			assertPlayerError(t, err, 9, message)
			if len(f.snapshot().Allocations) != 0 {
				t.Fatal("incompatible final pairing created an allocation")
			}
		})
	}
}

func TestAssignmentRPCProfileValidationPrecedesLookup(t *testing.T) {
	f := setup(t)
	hooks := playerHooks(t, f.m)
	ctx := context.WithValue(context.Background(), runtime.RUNTIME_CTX_USER_ID, "player-with-no-allocation")
	for _, name := range []string{"fleet_assignment_get_v1", "fleet_resume_v1"} {
		t.Run(name, func(t *testing.T) {
			for _, test := range []struct {
				payload, message string
			}{
				{`{"build_hash":"old-build","region":"local"}`, "fleet_build_mismatch"},
				{`{"build_hash":"test-build","region":"other-region"}`, "fleet_region_mismatch"},
				{`{"allocation_id":"missing-id","build_hash":"old-build"}`, "fleet_build_mismatch"},
				{`{"build_hash":"","region":"local"}`, "fleet_build_mismatch"},
				{`{"build_hash":null}`, "fleet_build_mismatch"},
				{`{"region":null}`, "fleet_region_mismatch"},
			} {
				before := f.snapshot()
				result, err := hooks.rpcs[name](ctx, nil, nil, nil, test.payload)
				assertPlayerError(t, err, 9, test.message)
				if result != "" || !reflect.DeepEqual(before, f.snapshot()) {
					t.Fatal("profile preflight must not modify allocations or mint a ticket")
				}
			}
			for _, payload := range []string{"", `{}`, `{"allocation_id":"missing-id"}`, `{"build_hash":"test-build","region":"local"}`, `{"build_hash":"test-build"}`, `{"region":"local"}`} {
				_, err := hooks.rpcs[name](ctx, nil, nil, nil, payload)
				assertPlayerError(t, err, 5, "assignment not found")
			}
		})
	}
}

func TestAssignmentRPCKeepsLegacySuccessAndOwnership(t *testing.T) {
	f := setup(t)
	hooks := playerHooks(t, f.m)
	allocationID, err := f.m.Allocate(context.Background(), []string{"player-1", "player-2"}, "profile-test")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), runtime.RUNTIME_CTX_USER_ID, "player-1")
	for _, name := range []string{"fleet_assignment_get_v1", "fleet_resume_v1"} {
		for _, request := range []map[string]string{
			{}, {"allocation_id": allocationID},
			{"allocation_id": allocationID, "build_hash": f.m.cfg.BuildHash, "region": f.m.cfg.Region},
		} {
			payload, _ := json.Marshal(request)
			result, err := hooks.rpcs[name](ctx, nil, nil, nil, string(payload))
			if err != nil {
				t.Fatalf("compatible or legacy %s request failed: %v", name, err)
			}
			var assignment Assignment
			if err := json.Unmarshal([]byte(result), &assignment); err != nil || assignment.AllocationID != allocationID || assignment.State != "waiting_capacity" || assignment.AdmissionToken != "" {
				t.Fatalf("unexpected existing assignment response: %v", err)
			}
		}
		foreign := context.WithValue(context.Background(), runtime.RUNTIME_CTX_USER_ID, "unrelated-player")
		payload, _ := json.Marshal(map[string]string{"allocation_id": allocationID, "build_hash": f.m.cfg.BuildHash, "region": f.m.cfg.Region})
		_, err := hooks.rpcs[name](foreign, nil, nil, nil, string(payload))
		assertPlayerError(t, err, 7, "forbidden")
	}
}

func TestAssignmentRPCAuthenticationAndMalformedPayload(t *testing.T) {
	f := setup(t)
	hooks := playerHooks(t, f.m)
	ctx := context.WithValue(context.Background(), runtime.RUNTIME_CTX_USER_ID, "player-1")
	for _, name := range []string{"fleet_assignment_get_v1", "fleet_resume_v1"} {
		_, err := hooks.rpcs[name](context.Background(), nil, nil, nil, `{"build_hash":"old-build"}`)
		assertPlayerError(t, err, 16, "user authentication required")
		for _, payload := range []string{`{"build_hash":`, `{"build_hash":12}`, `{"region":[]}`, `{"region":true}`} {
			_, err := hooks.rpcs[name](ctx, nil, nil, nil, payload)
			assertPlayerError(t, err, 3, "invalid payload")
		}
	}
}

func TestManualCompatibilityVersionsRemainOpaqueStrings(t *testing.T) {
	for _, version := range []string{"dm-v1", "1", strings.Repeat("a", 64)} {
		t.Run(version, func(t *testing.T) {
			cfg := testConfig()
			cfg.BuildHash = version
			manager, err := New(cfg, state.NewMemory(), &fakeProvider{})
			if err != nil {
				t.Fatalf("manual compatibility version rejected by config: %v", err)
			}
			hooks := playerHooks(t, manager)
			request := matchmakerEnvelope(version, cfg.Region)
			if result, err := hooks.before(context.Background(), nil, nil, nil, request); err != nil || result != request {
				t.Fatalf("compatible manual version rejected before queueing: %v", err)
			}
			ctx := context.WithValue(context.Background(), runtime.RUNTIME_CTX_USER_ID, "player-with-no-allocation")
			payload, _ := json.Marshal(map[string]string{"build_hash": version, "region": cfg.Region})
			_, err = hooks.rpcs["fleet_assignment_get_v1"](ctx, nil, nil, nil, string(payload))
			assertPlayerError(t, err, 5, "assignment not found")
		})
	}
}
