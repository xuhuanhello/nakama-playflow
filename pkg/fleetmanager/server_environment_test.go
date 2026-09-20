package fleetmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/xuhuanhello/nakama-playflow/internal/playflow"
	"github.com/xuhuanhello/nakama-playflow/internal/state"
)

func TestServerEnvironmentRejectsUnsafeOrAmbiguousInput(t *testing.T) {
	tooMany := make(map[string]string)
	for i := 0; i < 65; i++ {
		tooMany[fmt.Sprintf("GAME_%d", i)] = "x"
	}
	tooManyJSON, _ := json.Marshal(tooMany)
	cases := map[string]string{
		"array":              `[]`,
		"null object":        `null`,
		"number":             `{"GAME_PORT":7770}`,
		"null value":         `{"GAME_PORT":null}`,
		"object value":       `{"GAME_PORT":{}}`,
		"duplicate":          `{"GAME_PORT":"7770","GAME_PORT":"8880"}`,
		"trailing object":    `{"GAME_PORT":"7770"}{}`,
		"trailing invalid":   `{"GAME_PORT":"7770"} false`,
		"malformed":          `{"GAME_TOKEN":"fixture-sensitive-content",oops}`,
		"reserved":           `{"FLEET_WORKER_ID":"fixture-sensitive-content"}`,
		"reserved lowercase": `{"fleet_bootstrap_token":"fixture-sensitive-content"}`,
		"reserved mixed":     `{"Fleet_ADMISSION_KEY":"fixture-sensitive-content"}`,
		"empty name":         `{"":"x"}`,
		"digit first":        `{"9GAME":"x"}`,
		"name separator":     `{"GAME-PORT":"x"}`,
		"name equals":        `{"GAME=PORT":"x"}`,
		"name unicode":       `{"GAME_端口":"x"}`,
		"nul":                `{"GAME_TOKEN":"fixture-sensitive-content\u0000"}`,
		"invalid utf8":       "{\"GAME_TOKEN\":\"\xff\"}",
		"too many":           string(tooManyJSON),
		"value too long":     `{"GAME_DATA":"` + strings.Repeat("x", 8193) + `"}`,
		"name too long":      `{"` + strings.Repeat("X", 129) + `":"x"}`,
		"raw too long":       strings.Repeat(" ", 32769),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseServerEnvironment(raw)
			if err == nil {
				t.Fatal("unsafe server environment accepted")
			}
			if strings.Contains(err.Error(), "fixture-sensitive-content") {
				t.Fatal("configuration error exposed an environment value")
			}
		})
	}
}

func TestServerEnvironmentSizeBoundsAndProgrammaticValidation(t *testing.T) {
	values := map[string]string{"A": strings.Repeat("x", 8192), "B": strings.Repeat("x", 8192), "C": strings.Repeat("x", 8192), "D": strings.Repeat("x", 8163)}
	raw, _ := json.Marshal(values)
	if len(raw) != 32768 {
		t.Fatal("invalid boundary fixture")
	}
	if _, err := parseServerEnvironment(string(raw)); err != nil {
		t.Fatal(err)
	}
	values["D"] += "x"
	cfg := testConfig()
	cfg.ServerEnvironment = values
	if err := cfg.Validate(); err == nil {
		t.Fatal("programmatic config bypassed total environment limit")
	}
	for name, value := range map[string]string{"FLEET_CONTROL_URL": "https://other.invalid", "GAME_DATA": "\x00"} {
		cfg.ServerEnvironment = map[string]string{name: value}
		if err := cfg.Validate(); err == nil {
			t.Fatal("programmatic config bypassed environment validation")
		}
	}
	valid := make(map[string]string)
	for i := 0; i < 64; i++ {
		valid[fmt.Sprintf("GAME_%d", i)] = ""
	}
	cfg.ServerEnvironment = valid
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestFromEnvReadsServerEnvironment(t *testing.T) {
	for key, value := range map[string]string{
		"FLEET_DATABASE_URL": "postgres://local.invalid/fleet", "FLEET_PLAYFLOW_API_KEY": "test",
		"FLEET_CONTROL_URL": "https://control.invalid", "FLEET_BUILD_HASH": "test-build", "FLEET_REGION": "local",
		"FLEET_PROVIDER_VERSION": "1", "FLEET_MAX_ROOMS": "2", "FLEET_MIN_INSTANCES": "0", "FLEET_MAX_INSTANCES": "1",
		"FLEET_ADMIN_TOKEN": "test-admin-token-at-least-32-characters", "FLEET_SIGNING_KEY": encoded(testConfig().SigningKey),
		"FLEET_SERVER_ENV_JSON": `{"GAME_PORT":"7770","GAME_LABEL":"a $ value"}`,
	} {
		t.Setenv(key, value)
	}
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerEnvironment["GAME_PORT"] != "7770" || cfg.ServerEnvironment["GAME_LABEL"] != "a $ value" {
		t.Fatal("environment string values changed during configuration")
	}
	t.Setenv("FLEET_SERVER_ENV_JSON", `{"FLEET_CONTROL_URL":"https://other.invalid"}`)
	if _, err := FromEnv(); err == nil {
		t.Fatal("reserved environment accepted by FromEnv")
	}
}

type environmentProvider struct {
	*fakeProvider
	last playflow.StartRequest
}

func (p *environmentProvider) Start(ctx context.Context, req playflow.StartRequest) (playflow.Instance, error) {
	p.last = req
	return p.fakeProvider.Start(ctx, req)
}

func TestServerEnvironmentReachesProviderWithoutEnteringState(t *testing.T) {
	cfg := testConfig()
	cfg.MinInstances = 1
	cfg.ServerEnvironment = map[string]string{"GAME_PORT": "7770", "GAME_RESULT_TOKEN": "fixture-sensitive-content"}
	store := state.NewMemory()
	provider := &environmentProvider{fakeProvider: &fakeProvider{instances: map[string]playflow.Instance{}}}
	m, err := New(cfg, store, provider)
	if err != nil {
		t.Fatal(err)
	}
	// Neither the caller nor a returned config can mutate the active profile.
	cfg.ServerEnvironment["GAME_RESULT_TOKEN"] = "changed-after-new"
	returned := m.Config()
	returned.ServerEnvironment["GAME_PORT"] = "8880"
	returned.SigningKey[0]++
	if err := m.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.last.EnvironmentVariables["GAME_RESULT_TOKEN"] != "fixture-sensitive-content" || provider.last.EnvironmentVariables["GAME_PORT"] != "7770" {
		t.Fatal("provider did not receive the immutable custom environment")
	}
	if provider.last.EnvironmentVariables["FLEET_CONTROL_URL"] != cfg.ControlURL || provider.last.EnvironmentVariables["FLEET_MAX_ROOMS"] != "2" || provider.last.EnvironmentVariables["FLEET_WORKER_ID"] == "" {
		t.Fatal("custom environment displaced controller-owned values")
	}
	provider.last.EnvironmentVariables["GAME_PORT"] = "9990"
	if m.Config().ServerEnvironment["GAME_PORT"] != "7770" {
		t.Fatal("provider request mutation escaped into the active config")
	}
	snapshot, err := store.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(snapshot)
	metadata, _ := json.Marshal(provider.last.CustomData)
	if strings.Contains(string(data), "fixture-sensitive-content") || strings.Contains(string(metadata), "fixture-sensitive-content") {
		t.Fatal("custom environment secret leaked into durable state or public metadata")
	}
}

func TestServerEnvironmentFencesProfileChanges(t *testing.T) {
	cfg := testConfig()
	cfg.ServerEnvironment = map[string]string{"GAME_B": "second", "GAME_A": "first"}
	store := state.NewMemory()
	provider := &fakeProvider{instances: map[string]playflow.Instance{}}
	if _, err := New(cfg, store, provider); err != nil {
		t.Fatal(err)
	}
	reordered := testConfig()
	reordered.ServerEnvironment = map[string]string{"GAME_A": "first", "GAME_B": "second"}
	if _, err := New(reordered, store, provider); err != nil {
		t.Fatal("equivalent environment was treated as profile drift")
	}
	for _, environment := range []map[string]string{nil, {"GAME_A": "changed", "GAME_B": "second"}, {"GAME_A": "first", "GAME_B": "second", "GAME_C": "new"}} {
		changed := testConfig()
		changed.ServerEnvironment = environment
		if _, err := New(changed, store, provider); err == nil {
			t.Fatal("changed server environment reused an incompatible namespace")
		}
	}
}

func TestEmptyServerEnvironmentPreservesExistingProfile(t *testing.T) {
	store := state.NewMemory()
	// Fingerprint recorded before custom environment support for testConfig().
	if err := store.Update(context.Background(), func(s *state.State) error {
		s.Profile = "bb974596e09dadc67507bf4bc3920769e578d2407eecb325870d094126dd6667"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, environment := range []map[string]string{nil, {}} {
		cfg := testConfig()
		cfg.ServerEnvironment = environment
		if _, err := New(cfg, store, &fakeProvider{instances: map[string]playflow.Instance{}}); err != nil {
			t.Fatal("empty custom environment invalidated an existing namespace")
		}
	}
}
