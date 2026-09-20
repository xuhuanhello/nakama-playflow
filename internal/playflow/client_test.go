package playflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL+"/api", "secret-test-key", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestStartPayloadAndExternalPort(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/v3/servers/start" || r.Header.Get("api-key") != "secret-test-key" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["region"] != "sea" || body["version"] != float64(17) || body["ttl"] != float64(3600) || body["version_tag"] != "stable" {
			t.Errorf("invalid build payload: %v", body)
		}
		if body["environment_variables"].(map[string]any)["FLEET_WORKER_TOKEN"] != "worker-secret" || body["custom_data"].(map[string]any)["allocation_id"] != "allocation-1" {
			t.Error("metadata/environment mismatch")
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"instance_id":"instance-1","status":"launching","version":17,"network_ports":[{"name":"game","host":"203.0.113.1","internal_port":7770,"external_port":32701,"protocol":"udp","tls_enabled":false}]}`)
	})
	instance, err := client.Start(context.Background(), StartRequest{
		Name: "dm-worker", Region: "sea", VersionTag: "stable", Version: 17, TTL: 3600,
		CustomData:           map[string]any{"allocation_id": "allocation-1"},
		EnvironmentVariables: map[string]string{"FLEET_WORKER_TOKEN": "worker-secret"},
		PortConfigs:          []PortConfig{{Name: "game", InternalPort: 7770, Protocol: "udp"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	port := instance.NetworkPorts[0]
	if port.ExternalPort != 32701 || port.InternalPort != 7770 || port.Host != "203.0.113.1" {
		t.Fatalf("wrong public endpoint: %+v", port)
	}
}

func TestListFollowsHasMoreEvenForFilteredShortOrEmptyPage(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("include_launching") != "true" || r.URL.Query().Get("include_pool") != "false" || r.URL.Query().Get("limit") != "100" {
			t.Error("list must include launching and exclude pool")
		}
		switch r.URL.Query().Get("offset") {
		case "0":
			fmt.Fprint(w, `{"total":99,"offset":0,"limit":2,"has_more":true,"servers":[{"instance_id":"a","status":"running"}]}`)
		case "2":
			fmt.Fprint(w, `{"total":99,"offset":2,"limit":2,"has_more":true,"servers":[]}`)
		case "4":
			fmt.Fprint(w, `{"total":99,"offset":4,"limit":2,"has_more":false,"servers":[{"instance_id":"a","status":"running"},{"instance_id":"b","status":"launching"}]}`)
		default:
			t.Error("unexpected pagination offset")
			w.WriteHeader(400)
		}
	})
	instances, err := client.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 2 || instances[1].Status != "launching" || calls.Load() != 3 {
		t.Fatalf("incomplete or duplicated result: %+v calls=%d", instances, calls.Load())
	}
}

func TestListIsBoundedAndDoesNotReturnPartialResults(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		fmt.Fprintf(w, `{"total":999999,"offset":%d,"limit":100,"has_more":true,"servers":[]}`, offset)
	})
	instances, err := client.List(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Reason != "pagination_limit_exceeded" || instances != nil || calls.Load() != maxListPages {
		t.Fatalf("unbounded or partial pagination: %v %d", err, calls.Load())
	}
}

func TestListRejectsMalformedPagination(t *testing.T) {
	for _, payload := range []string{
		`{"offset":0,"limit":0,"has_more":true,"servers":[]}`,
		`{"offset":1,"limit":100,"has_more":false,"servers":[]}`,
		`{"offset":0,"limit":100,"servers":[]}`,
	} {
		t.Run(payload, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, payload) })
			if instances, err := client.List(context.Background()); err == nil || instances != nil {
				t.Fatalf("accepted invalid pagination: %+v %v", instances, err)
			}
		})
	}
}

func TestStartAmbiguousFailuresNeverAutomaticallyRetry(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusRequestTimeout} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var calls atomic.Int32
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
				fmt.Fprint(w, `{"error":"secret-test-key worker-secret"}`)
			})
			_, err := client.Start(context.Background(), StartRequest{Name: "dm", Region: "sea"})
			if !IsOutcomeUnknown(err) || IsRetryable(err) || calls.Load() != 1 {
				t.Fatalf("unsafe retry classification: %v", err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("response leaked in error")
			}
		})
	}
}

func TestRateLimitIsRetryableAndExposesDelay(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := client.Start(context.Background(), StartRequest{Name: "dm", Region: "sea"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !IsRetryable(err) || IsOutcomeUnknown(err) || apiErr.RetryAfter != 3*time.Second {
		t.Fatalf("bad retry classification: %v", err)
	}
}

func TestGetAndIdempotentStopNotFound(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	_, err := client.Get(context.Background(), "missing-1")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 || IsRetryable(err) {
		t.Fatal(err)
	}
	if err := client.Stop(context.Background(), "missing-1"); err != nil {
		t.Fatal(err)
	}
}

func TestValidationPreventsUnsafeURLsAndIDs(t *testing.T) {
	for _, base := range []string{"http://example.com/api", "https://user:password@example.com/api", "https://example.com/api?token=secret", "https://example.com/api#token", "https://example.com/api/../else", "https://example.com/api%2fv3", "file:///api"} {
		if _, err := NewClient(base, "key", nil); err == nil {
			t.Errorf("unsafe URL accepted: %s", base)
		}
	}
	for _, base := range []string{"", "https://example.com/api/", "http://localhost:8090/api", "http://playflow-mock:8090/api", "http://127.0.0.1:8090/api", "http://[::1]:8090/api"} {
		if _, err := NewClient(base, "key", nil); err != nil {
			t.Errorf("valid URL rejected: %s %v", base, err)
		}
	}
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid id reached network") })
	for _, id := range []string{"", "../x", "id/other", "id?api-key=secret", "id#fragment", "x%2fy"} {
		if _, err := client.Get(context.Background(), id); err == nil {
			t.Errorf("unsafe id accepted: %s", id)
		}
		if err := client.Stop(context.Background(), id); err == nil {
			t.Errorf("unsafe id accepted: %s", id)
		}
	}
}

func TestRedirectDoesNotForwardCredentials(t *testing.T) {
	var forwarded atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1) }))
	defer target.Close()
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	})
	_, err := client.Get(context.Background(), "a")
	if err == nil || forwarded.Load() != 0 {
		t.Fatalf("redirect followed: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransportErrorRedactionAndPreCancelledContext(t *testing.T) {
	var calls atomic.Int32
	original := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, fmt.Errorf("leaked %s", r.Header.Get("api-key"))
	})}
	client, err := NewClient("", "secret-key-never-log", original)
	if err != nil {
		t.Fatal(err)
	}
	if original.Timeout != 0 {
		t.Fatal("caller client was mutated")
	}
	_, err = client.Start(context.Background(), StartRequest{Name: "dm", Region: "sea"})
	if !IsOutcomeUnknown(err) || strings.Contains(err.Error(), "secret-key") || errors.Unwrap(err) != nil {
		t.Fatalf("unsafe error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.Start(ctx, StartRequest{Name: "dm", Region: "sea"})
	if IsOutcomeUnknown(err) || !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatalf("cancelled before send classified as uncertain: %v", err)
	}
}

func TestTimeoutIsBoundedAndCreateRemainsUncertain(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(300 * time.Millisecond):
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL+"/api", "key", &http.Client{Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = client.Start(context.Background(), StartRequest{Name: "dm", Region: "sea"})
	if !IsOutcomeUnknown(err) || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("timeout classification: %v", err)
	}
}

func TestInvalidAndOversizedCreateResponsesAreUncertain(t *testing.T) {
	for name, body := range map[string]string{
		"malformed":     `{"instance_id":`,
		"missing_id":    `{"status":"running"}`,
		"multiple_json": `{"instance_id":"a","status":"running"}{}`,
		"oversized":     strings.Repeat("x", maxBodyBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
			_, err := client.Start(context.Background(), StartRequest{Name: "dm", Region: "sea"})
			if !IsOutcomeUnknown(err) || IsRetryable(err) {
				t.Fatalf("create response needs reconciliation: %v", err)
			}
		})
	}
}

func TestRetryAfterIsBounded(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, test := range []struct {
		input string
		want  time.Duration
	}{
		{"-1", 0}, {"not-a-date", 0}, {"9999999999", time.Hour}, {"2", 2 * time.Second},
		{now.Add(4 * time.Second).Format(http.TimeFormat), 4 * time.Second},
	} {
		if got := parseRetryAfter(test.input, now); got != test.want {
			t.Errorf("%q: got %s want %s", test.input, got, test.want)
		}
	}
}
