package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xuhuanhello/nakama-playflow/internal/playflow"
)

func mockCall(t *testing.T, server *mockServer, method, path string, payload any, key string) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(data))
	r.Header.Set("api-key", key)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	return w
}

func TestMockLifecycleAndIntrospectionIsolation(t *testing.T) {
	current := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	server := newMockServer("test-key", true, "127.0.0.1", faults{ReadyDelayMS: 500})
	server.now = func() time.Time { return current }
	created := mockCall(t, server, "POST", "/api/v3/servers/start", playflow.StartRequest{
		Name: "worker-1", Region: "sea", TTL: 60,
		CustomData:           map[string]any{"request_id": "request-1"},
		EnvironmentVariables: map[string]string{"FLEET_WORKER_TOKEN": "secret-worker-credential"},
	}, "test-key")
	if created.Code != 201 {
		t.Fatalf("start: %s", created.Body)
	}
	var instance playflow.Instance
	if err := json.Unmarshal(created.Body.Bytes(), &instance); err != nil {
		t.Fatal(err)
	}
	if instance.Status != "launching" || instance.NetworkPorts[0].Name != "game" || instance.NetworkPorts[0].ExternalPort == instance.NetworkPorts[0].InternalPort {
		t.Fatalf("bad initial instance: %+v", instance)
	}
	if strings.Contains(created.Body.String(), "secret-worker") {
		t.Fatal("credential leaked in public start response")
	}
	listed := mockCall(t, server, "GET", "/api/v3/servers", nil, "test-key")
	if strings.Contains(listed.Body.String(), instance.InstanceID) {
		t.Fatal("launching included by default")
	}
	listed = mockCall(t, server, "GET", "/api/v3/servers?include_launching=true", nil, "test-key")
	if !strings.Contains(listed.Body.String(), instance.InstanceID) || strings.Contains(listed.Body.String(), "secret-worker") {
		t.Fatal("list contract violated")
	}
	introspection := mockCall(t, server, "GET", "/__mock/instances", nil, "test-key")
	if !strings.Contains(introspection.Body.String(), "secret-worker-credential") {
		t.Fatal("simulator cannot bootstrap")
	}
	if got := mockCall(t, server, "GET", "/__mock/instances", nil, "wrong"); got.Code != 401 {
		t.Fatal("introspection unauthenticated")
	}
	current = current.Add(time.Second)
	got := mockCall(t, server, "GET", "/api/v3/servers/"+instance.InstanceID, nil, "test-key")
	if err := json.Unmarshal(got.Body.Bytes(), &instance); err != nil {
		t.Fatal(err)
	}
	if instance.Status != "running" || instance.StartedAt == "" {
		t.Fatalf("not ready: %+v", instance)
	}
	current = current.Add(time.Minute)
	got = mockCall(t, server, "GET", "/api/v3/servers/"+instance.InstanceID, nil, "test-key")
	if err := json.Unmarshal(got.Body.Bytes(), &instance); err != nil {
		t.Fatal(err)
	}
	if instance.Status != "stopped" || len(instance.NetworkPorts) != 0 {
		t.Fatalf("TTL not enforced: %+v", instance)
	}
	server.testMode = false
	if got := mockCall(t, server, "GET", "/__mock/instances", nil, "test-key"); got.Code != 404 {
		t.Fatal("introspection enabled outside test mode")
	}
}

func TestMockClientReconcilesAmbiguousCreateAndStopFailure(t *testing.T) {
	handler := newMockServer("test-key", true, "127.0.0.1", faults{ReadyDelayMS: 3600000, AmbiguousCreates: 1, StopFailures: 1})
	server := httptest.NewServer(handler)
	defer server.Close()
	client, err := playflow.NewClient(server.URL+"/api", "test-key", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Start(context.Background(), playflow.StartRequest{Name: "worker", Region: "sea", CustomData: map[string]any{"request_id": "unique-request"}})
	if !playflow.IsOutcomeUnknown(err) {
		t.Fatalf("mock did not inject ambiguity: %v", err)
	}
	instances, err := client.List(context.Background())
	if err != nil || len(instances) != 1 || instances[0].CustomData["request_id"] != "unique-request" {
		t.Fatalf("uncertain create could not be found: %+v %v", instances, err)
	}
	id := instances[0].InstanceID
	if err := client.Stop(context.Background(), id); !playflow.IsRetryable(err) {
		t.Fatalf("missing stop failure: %v", err)
	}
	instance, err := client.Get(context.Background(), id)
	if err != nil || instance.Status != "launching" {
		t.Fatalf("failed stop destroyed instance: %+v %v", instance, err)
	}
	if err := client.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := client.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	instance, err = client.Get(context.Background(), id)
	if err != nil || instance.Status != "stopped" {
		t.Fatalf("stop did not complete: %+v %v", instance, err)
	}
}

func TestMockPaginationAndFaultControls(t *testing.T) {
	server := newMockServer("test-key", true, "127.0.0.1", faults{})
	for i := 0; i < 3; i++ {
		result := mockCall(t, server, "POST", "/api/v3/servers/start", playflow.StartRequest{Name: "worker", Region: "sea"}, "test-key")
		if result.Code != 201 {
			t.Fatal(result.Body)
		}
	}
	page := mockCall(t, server, "GET", "/api/v3/servers?limit=2&offset=0", nil, "test-key")
	var result struct {
		Servers []playflow.Instance `json:"servers"`
		HasMore bool                `json:"has_more"`
		Total   int                 `json:"total"`
	}
	if err := json.Unmarshal(page.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.HasMore || len(result.Servers) != 2 || result.Total != 3 {
		t.Fatalf("wrong first page: %+v", result)
	}
	page = mockCall(t, server, "GET", "/api/v3/servers?limit=2&offset=2", nil, "test-key")
	if err := json.Unmarshal(page.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.HasMore || len(result.Servers) != 1 {
		t.Fatalf("wrong second page: %+v", result)
	}
	changed := mockCall(t, server, "POST", "/__mock/faults", map[string]int{"rate_limit_requests": 1}, "test-key")
	if changed.Code != 200 {
		t.Fatal(changed.Body)
	}
	if got := mockCall(t, server, "GET", "/api/v3/servers", nil, "test-key"); got.Code != 429 || got.Header().Get("Retry-After") != "1" {
		t.Fatal("missing rate limit fault")
	}
	if got := mockCall(t, server, "GET", "/api/v3/servers", nil, "test-key"); got.Code != 200 {
		t.Fatal("rate limit was not consumed")
	}
	if got := mockCall(t, server, "POST", "/__mock/faults", map[string]int{"ready_delay_ms": -1}, "test-key"); got.Code != 400 {
		t.Fatal("invalid fault accepted")
	}
}

func TestMockAuthAndNormalGetNeverExposeEnvironment(t *testing.T) {
	handler := newMockServer("test-key", true, "127.0.0.1", faults{})
	server := httptest.NewServer(handler)
	defer server.Close()
	client, err := playflow.NewClient(server.URL+"/api", "test-key", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	instance, err := client.Start(context.Background(), playflow.StartRequest{Name: "dm", Region: "sea", EnvironmentVariables: map[string]string{"FLEET_SECRET": "private-token"}})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest("GET", server.URL+"/api/v3/servers/"+instance.InstanceID, nil)
	request.Header.Set("api-key", "test-key")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if strings.Contains(string(body), "private-token") || strings.Contains(string(body), "environment_variables") {
		t.Fatal("private environment exposed")
	}
	request.Header.Del("api-key")
	response2, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response2.Body.Close()
	if response2.StatusCode != 401 {
		t.Fatal("auth not required")
	}
}
