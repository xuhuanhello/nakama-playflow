package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xuhuanhello/nakama-playflow/internal/playflow"
)

type faults struct {
	ReadyDelayMS      int `json:"ready_delay_ms"`
	AmbiguousCreates  int `json:"ambiguous_creates"`
	RateLimitRequests int `json:"rate_limit_requests"`
	StopFailures      int `json:"stop_failures"`
}

type record struct {
	Instance             playflow.Instance
	EnvironmentVariables map[string]string
	ReadyAt              time.Time
	CreatedAt            time.Time
}

type mockServer struct {
	mu         sync.Mutex
	apiKey     [32]byte
	testMode   bool
	publicHost string
	faults     faults
	instances  map[string]*record
	nextID     int
	now        func() time.Time
}

func newMockServer(apiKey string, testMode bool, publicHost string, settings faults) *mockServer {
	return &mockServer{
		apiKey: sha256.Sum256([]byte(apiKey)), testMode: testMode, publicHost: publicHost,
		faults: settings, instances: make(map[string]*record), now: time.Now,
	}
}

func (s *mockServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	isControl := strings.HasPrefix(r.URL.Path, "/__mock/")
	if isControl && !s.testMode {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	key := sha256.Sum256([]byte(r.Header.Get("api-key")))
	if subtle.ConstantTimeCompare(key[:], s.apiKey[:]) != 1 {
		writeError(w, http.StatusUnauthorized, "invalid API key")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.advance()
	if isControl {
		s.control(w, r)
		return
	}
	if s.faults.RateLimitRequests > 0 && strings.HasPrefix(r.URL.Path, "/api/v3/") {
		s.faults.RateLimitRequests--
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "injected rate limit")
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v3/servers/start":
		s.start(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/servers":
		s.list(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/v3/servers/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/v3/servers/")
		entry, ok := s.instances[id]
		if !ok {
			writeError(w, http.StatusNotFound, "server not found")
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, wireInstance(entry.Instance))
		case http.MethodDelete:
			if s.faults.StopFailures > 0 {
				s.faults.StopFailures--
				writeError(w, http.StatusServiceUnavailable, "injected stop failure")
				return
			}
			s.stop(entry)
			writeJSON(w, http.StatusOK, map[string]string{"status": "Server stopped successfully"})
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (s *mockServer) start(w http.ResponseWriter, r *http.Request) {
	var input playflow.StartRequest
	if err := decodeRequest(r, &input); err != nil || input.Name == "" || input.Region == "" || input.Version < 0 ||
		(input.TTL != 0 && (input.TTL < 60 || input.TTL > 86400)) {
		writeError(w, http.StatusBadRequest, "invalid start request")
		return
	}
	if s.nextID >= 10000 {
		writeError(w, http.StatusTooManyRequests, "mock instance limit reached; restart the mock")
		return
	}
	if len(input.PortConfigs) == 0 {
		input.PortConfigs = []playflow.PortConfig{{Name: "game", InternalPort: 7770, Protocol: "udp"}}
	}
	if len(input.PortConfigs) > 16 {
		writeError(w, http.StatusBadRequest, "too many port configurations")
		return
	}
	for _, port := range input.PortConfigs {
		if port.Name == "" || port.InternalPort < 1 || port.InternalPort > 65535 ||
			(port.Protocol != "udp" && port.Protocol != "tcp") || (port.TLSEnabled && port.Protocol != "tcp") {
			writeError(w, http.StatusBadRequest, "invalid port configuration")
			return
		}
	}
	s.nextID++
	id := fmt.Sprintf("00000000-0000-4000-8000-%012d", s.nextID)
	now := s.now().UTC()
	if input.ComputeSize == "" {
		input.ComputeSize = "small"
	}
	if input.VersionTag == "" {
		input.VersionTag = "default"
	}
	if input.Version == 0 {
		input.Version = 1
	}
	if input.MatchID == "" {
		input.MatchID = "mock-match-" + strconv.Itoa(s.nextID)
	}
	ports := make([]playflow.Port, 0, len(input.PortConfigs))
	for index, port := range input.PortConfigs {
		ports = append(ports, playflow.Port{
			Name: port.Name, InternalPort: port.InternalPort, Protocol: port.Protocol,
			Host: s.publicHost, ExternalPort: 30000 + ((s.nextID-1)*16+index)%30000, TLSEnabled: port.TLSEnabled,
		})
	}
	entry := &record{
		Instance: playflow.Instance{
			InstanceID: id, Name: input.Name, Status: "launching", NetworkPorts: ports,
			StartupArgs: input.StartupArgs, ServiceType: "match_based", ComputeSize: input.ComputeSize,
			Region: input.Region, VersionTag: input.VersionTag, Version: input.Version,
			AutoRestart: input.AutoRestart, CustomData: input.CustomData, TTL: input.TTL,
			MatchID: input.MatchID, CreatedAt: now.Format(time.RFC3339Nano), UpdatedAt: now.Format(time.RFC3339Nano),
		},
		EnvironmentVariables: input.EnvironmentVariables,
		CreatedAt:            now, ReadyAt: now.Add(time.Duration(s.faults.ReadyDelayMS) * time.Millisecond),
	}
	s.instances[id] = entry
	s.advance()
	if s.faults.AmbiguousCreates > 0 {
		s.faults.AmbiguousCreates--
		// The instance really exists, even though the caller sees a failure.
		// Reconciliation must find its custom_data correlation ID before retry.
		writeError(w, http.StatusServiceUnavailable, "injected ambiguous create; reconcile before retry")
		return
	}
	writeJSON(w, http.StatusCreated, wireInstance(entry.Instance))
}

func (s *mockServer) list(w http.ResponseWriter, r *http.Request) {
	limit, offset := 50, 0
	if input := r.URL.Query().Get("limit"); input != "" {
		value, err := strconv.Atoi(input)
		if err != nil || value < 1 || value > 100 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = value
	}
	if input := r.URL.Query().Get("offset"); input != "" {
		value, err := strconv.Atoi(input)
		if err != nil || value < 0 {
			writeError(w, http.StatusBadRequest, "invalid offset")
			return
		}
		offset = value
	}
	includeLaunching := r.URL.Query().Get("include_launching") == "true"
	ids := make([]string, 0, len(s.instances))
	for id, entry := range s.instances {
		if entry.Instance.Status == "running" || (includeLaunching && entry.Instance.Status == "launching") {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	items := make([]map[string]any, 0, limit)
	end := offset + limit
	if end < offset || end > len(ids) {
		end = len(ids)
	}
	for index := offset; index < end; index++ {
		items = append(items, wireInstance(s.instances[ids[index]].Instance))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total": len(ids), "servers": items, "limit": limit, "offset": offset, "has_more": end < len(ids),
	})
}

func (s *mockServer) stop(entry *record) {
	if entry.Instance.Status == "stopped" {
		return
	}
	entry.Instance.Status = "stopped"
	entry.Instance.StoppedAt = s.now().UTC().Format(time.RFC3339Nano)
	entry.Instance.UpdatedAt = entry.Instance.StoppedAt
	entry.Instance.NetworkPorts = []playflow.Port{}
}

func (s *mockServer) advance() {
	now := s.now()
	for _, entry := range s.instances {
		if entry.Instance.Status == "stopped" {
			continue
		}
		if entry.Instance.TTL > 0 && !now.Before(entry.CreatedAt.Add(time.Duration(entry.Instance.TTL)*time.Second)) {
			s.stop(entry)
			continue
		}
		if entry.Instance.Status == "launching" && !now.Before(entry.ReadyAt) {
			entry.Instance.Status = "running"
			entry.Instance.StartedAt = entry.ReadyAt.UTC().Format(time.RFC3339Nano)
			entry.Instance.UpdatedAt = entry.Instance.StartedAt
		}
	}
}

func (s *mockServer) control(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/__mock/faults" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.faults)
	case r.URL.Path == "/__mock/faults" && r.Method == http.MethodPost:
		next := s.faults
		if err := decodeRequest(r, &next); err != nil || next.ReadyDelayMS < 0 || next.ReadyDelayMS > 3600000 ||
			next.AmbiguousCreates < 0 || next.RateLimitRequests < 0 || next.StopFailures < 0 {
			writeError(w, http.StatusBadRequest, "invalid fault settings")
			return
		}
		s.faults = next
		writeJSON(w, http.StatusOK, s.faults)
	case r.URL.Path == "/__mock/instances" && r.Method == http.MethodGet:
		ids := make([]string, 0, len(s.instances))
		for id := range s.instances {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		instances := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			entry := s.instances[id]
			item := wireInstance(entry.Instance)
			// This field is available only on authenticated test introspection.
			// It is not part of PlayFlow's public instance response.
			item["environment_variables"] = entry.EnvironmentVariables
			instances = append(instances, item)
		}
		writeJSON(w, http.StatusOK, map[string]any{"instances": instances})
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func decodeRequest(r *http.Request, target any) error {
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return fmt.Errorf("invalid request body")
	}
	return json.Unmarshal(data, target)
}

// Match nullable fields in the real v3 API while using ergonomic Go values.
func wireInstance(instance playflow.Instance) map[string]any {
	data, _ := json.Marshal(instance)
	var value map[string]any
	_ = json.Unmarshal(data, &value)
	for _, key := range []string{"startup_args", "started_at", "stopped_at", "pool_claimed_at", "match_id"} {
		if value[key] == "" {
			value[key] = nil
		}
	}
	for _, key := range []string{"ttl", "version"} {
		if value[key] == float64(0) {
			value[key] = nil
		}
	}
	return value
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message, "status": status})
}
