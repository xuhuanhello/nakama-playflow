package playflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultBaseURL = "https://api.computeflow.cloud/api"
	requestTimeout = 15 * time.Second
	listTimeout    = 90 * time.Second
	maxBodyBytes   = 4 << 20
	pageSize       = 100
	maxListPages   = 100
)

// APIError contains safe, structured diagnostics. It never includes request
// payloads, API keys, raw provider response bodies or transport error strings.
// StatusCode is zero when an HTTP response was not received.
type APIError struct {
	Operation      string
	StatusCode     int
	Reason         string
	RetryAfter     time.Duration
	OutcomeUnknown bool
	cause          error
}

func (e *APIError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("playflow %s: %s (HTTP %d)", e.Operation, e.Reason, e.StatusCode)
	}
	return fmt.Sprintf("playflow %s: %s", e.Operation, e.Reason)
}

// Unwrap exposes context cancellation/deadline only, never raw transport errors.
func (e *APIError) Unwrap() error { return e.cause }

// Retryable means a new attempt is safe after caller-controlled backoff.
// An uncertain create must instead be reconciled using its unique CustomData
// correlation ID. A retry could otherwise create and bill for a second server.
func (e *APIError) Retryable() bool {
	if e.OutcomeUnknown || errors.Is(e.cause, context.Canceled) {
		return false
	}
	return e.Reason == "transport_error" || e.StatusCode == http.StatusRequestTimeout ||
		e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

func IsRetryable(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Retryable()
}

func IsOutcomeUnknown(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.OutcomeUnknown
}

// Client is safe for concurrent calls. Each request has a deadline, redirects
// are refused, and Start never retries automatically.
type Client struct {
	baseURL *url.URL
	apiKey  string
	http    *http.Client
}

// NewClient requires HTTPS except for loopback addresses and the local compose
// service "playflow-mock". The base URL includes /api, not /v3. Empty baseURL
// selects DefaultBaseURL. The supplied HTTP client is copied before use.
func NewClient(baseURL, apiKey string, httpClient *http.Client) (*Client, error) {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" {
		return nil, errors.New("playflow: invalid base URL")
	}
	host := strings.ToLower(u.Hostname())
	ip := net.ParseIP(host)
	local := host == "localhost" || host == "playflow-mock" || (ip != nil && ip.IsLoopback())
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		return nil, errors.New("playflow: base URL requires HTTPS (local mock may use HTTP)")
	}
	if strings.TrimRight(u.Path, "/") != "/api" {
		return nil, errors.New("playflow: base URL path must be /api")
	}
	if apiKey == "" || strings.ContainsAny(apiKey, "\r\n") {
		return nil, errors.New("playflow: API key is missing or invalid")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	client := http.Client{Timeout: requestTimeout}
	if httpClient != nil {
		client = *httpClient
		if client.Timeout <= 0 || client.Timeout > requestTimeout {
			client.Timeout = requestTimeout
		}
	}
	// Even same-host redirects are unnecessary for this API. Explicit refusal
	// also prevents forwarding api-key headers to a third-party host.
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{baseURL: u, apiKey: apiKey, http: &client}, nil
}

func (c *Client) Start(ctx context.Context, input StartRequest) (Instance, error) {
	if strings.TrimSpace(input.Name) == "" || strings.TrimSpace(input.Region) == "" {
		return Instance{}, &APIError{Operation: "start", Reason: "name_and_region_required"}
	}
	if input.Version < 0 || input.TTL != 0 && (input.TTL < 60 || input.TTL > 86400) {
		return Instance{}, &APIError{Operation: "start", Reason: "invalid_version_or_ttl"}
	}
	for _, port := range input.PortConfigs {
		if strings.TrimSpace(port.Name) == "" || port.InternalPort < 1 || port.InternalPort > 65535 ||
			(port.Protocol != "udp" && port.Protocol != "tcp") || (port.TLSEnabled && port.Protocol != "tcp") {
			return Instance{}, &APIError{Operation: "start", Reason: "invalid_port_config"}
		}
	}
	var instance Instance
	if err := c.request(ctx, "start", http.MethodPost, "/v3/servers/start", nil, input, &instance); err != nil {
		return Instance{}, err
	}
	if !validID(instance.InstanceID) || instance.Status == "" {
		return Instance{}, &APIError{Operation: "start", Reason: "invalid_instance_response", OutcomeUnknown: true}
	}
	return instance, nil
}

func (c *Client) Get(ctx context.Context, id string) (Instance, error) {
	if !validID(id) {
		return Instance{}, &APIError{Operation: "get", Reason: "invalid_instance_id"}
	}
	var instance Instance
	if err := c.request(ctx, "get", http.MethodGet, "/v3/servers/"+id, nil, nil, &instance); err != nil {
		return Instance{}, err
	}
	if instance.InstanceID != id || instance.Status == "" {
		return Instance{}, &APIError{Operation: "get", Reason: "invalid_instance_response"}
	}
	return instance, nil
}

// List returns active instances including those still launching. Unclaimed
// provider pool machines are excluded. Pagination uses the provider's declared
// offset+limit and has_more, not array length or total: filtering and concurrent
// updates can produce short or empty intermediate pages. This API has no
// snapshot cursor; repeated records are deduplicated. Reconciliation must not
// infer a missing instance has stopped without a confirming Get.
func (c *Client) List(ctx context.Context) ([]Instance, error) {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	instances := make([]Instance, 0)
	seen := make(map[string]struct{})
	offset := 0
	for page := 0; page < maxListPages; page++ {
		query := url.Values{"include_launching": {"true"}, "include_pool": {"false"}, "limit": {strconv.Itoa(pageSize)}, "offset": {strconv.Itoa(offset)}}
		var result listResponse
		if err := c.request(ctx, "list", http.MethodGet, "/v3/servers", query, nil, &result); err != nil {
			return nil, err // Never hand an incomplete snapshot to reconciliation.
		}
		if result.HasMore == nil || result.Offset != offset || result.Limit < 1 || result.Limit > pageSize || len(result.Servers) > result.Limit {
			return nil, &APIError{Operation: "list", Reason: "invalid_pagination"}
		}
		for _, instance := range result.Servers {
			if !validID(instance.InstanceID) || instance.Status == "" {
				return nil, &APIError{Operation: "list", Reason: "invalid_instance_response"}
			}
			if _, exists := seen[instance.InstanceID]; !exists {
				instances = append(instances, instance)
				seen[instance.InstanceID] = struct{}{}
			}
		}
		if !*result.HasMore {
			return instances, nil
		}
		offset += result.Limit
	}
	return nil, &APIError{Operation: "list", Reason: "pagination_limit_exceeded"}
}

// Stop requests shutdown once. A missing instance (404) is already stopped for
// this operation. Callers retain state and reconcile transient failures.
func (c *Client) Stop(ctx context.Context, id string) error {
	if !validID(id) {
		return &APIError{Operation: "stop", Reason: "invalid_instance_id"}
	}
	err := c.request(ctx, "stop", http.MethodDelete, "/v3/servers/"+id, nil, nil, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return nil
	}
	return err
}

func validID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, char := range id {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_') {
			return false
		}
	}
	return true
}

func (c *Client) request(ctx context.Context, operation, method, path string, query url.Values, input, output any) error {
	if err := ctx.Err(); err != nil {
		return &APIError{Operation: operation, Reason: "context_ended", cause: err}
	}
	var body []byte
	if input != nil {
		var err error
		body, err = json.Marshal(input)
		if err != nil || len(body) > maxBodyBytes {
			return &APIError{Operation: operation, Reason: "invalid_request_body"}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	u := *c.baseURL
	u.Path += path
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return &APIError{Operation: operation, Reason: "invalid_request"}
	}
	req.Header.Set("api-key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(req)
	if err != nil {
		var cause error
		if errors.Is(err, context.Canceled) {
			cause = context.Canceled
		} else if errors.Is(err, context.DeadlineExceeded) {
			cause = context.DeadlineExceeded
		}
		return &APIError{Operation: operation, Reason: "transport_error", OutcomeUnknown: method == http.MethodPost, cause: cause}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Raw provider bodies can echo submitted environment variables or keys.
		// Keep only status and bounded Retry-After information in errors.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxBodyBytes))
		return &APIError{
			Operation: operation, StatusCode: response.StatusCode, Reason: "provider_rejected_request",
			RetryAfter:     parseRetryAfter(response.Header.Get("Retry-After"), time.Now()),
			OutcomeUnknown: method == http.MethodPost && (response.StatusCode >= 500 || response.StatusCode == http.StatusRequestTimeout),
		}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes+1))
	if err != nil || len(data) > maxBodyBytes {
		return &APIError{Operation: operation, StatusCode: response.StatusCode, Reason: "invalid_response_body", OutcomeUnknown: method == http.MethodPost}
	}
	if output != nil {
		if err := json.Unmarshal(data, output); err != nil {
			return &APIError{Operation: operation, StatusCode: response.StatusCode, Reason: "invalid_response_json", OutcomeUnknown: method == http.MethodPost}
		}
	}
	return nil
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds > 3600 {
			return time.Hour
		}
		return time.Duration(seconds) * time.Second
	}
	if deadline, err := http.ParseTime(value); err == nil && deadline.After(now) {
		duration := deadline.Sub(now)
		if duration > time.Hour {
			return time.Hour
		}
		return duration
	}
	return 0
}
