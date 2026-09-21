package fleetmanager

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/heroiclabs/nakama-common/rtapi"
	"github.com/heroiclabs/nakama-common/runtime"
)

// Register installs the standard manager and game bridge. This application owns
// MatchmakerAdd before hook and the single matchmaker-matched hook; existing
// games must compose these hooks.
func Register(ctx context.Context, logger runtime.Logger, db *sql.DB, nk runtime.NakamaModule, initializer runtime.Initializer, m *Manager) error {
	if err := initializer.RegisterFleetManager(m); err != nil {
		return err
	}
	handler := m.HTTPHandler()
	// Nakama's router treats RegisterHttp paths as exact Gorilla routes.
	for _, path := range []string{"/fleet/v1/agent/bootstrap", "/fleet/v1/agent/heartbeat", "/fleet/v1/admin/status", "/fleet/v1/admin/drain", "/fleet/v1/admin/allocate", "/fleet/v1/admin/retry-creation"} {
		if err := initializer.RegisterHttp(path, func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }); err != nil {
			return err
		}
	}
	if err := registerPlayerHooks(initializer, m); err != nil {
		return err
	}
	background, cancel := context.WithCancel(context.Background())
	if err := initializer.RegisterShutdown(func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule) { cancel() }); err != nil {
		cancel()
		return err
	}
	go m.Run(background, func(err error) { logger.Error("fleet reconciliation: %v", err) })
	logger.Info("PlayFlow FleetManager registered; namespace=%s mode=%s rooms=%d", m.cfg.DeploymentID, m.cfg.Mode, m.cfg.MaxRooms)
	return nil
}

// Register these hooks together so incompatible tickets are rejected before
// queue admission, while the matched hook still validates the final pairing.
func registerPlayerHooks(initializer runtime.Initializer, m *Manager) error {
	if err := initializer.RegisterBeforeRt("MatchmakerAdd", func(ctx context.Context, logger runtime.Logger, db *sql.DB, nk runtime.NakamaModule, in *rtapi.Envelope) (*rtapi.Envelope, error) {
		request := in.GetMatchmakerAdd()
		if request == nil {
			return nil, runtime.NewError("invalid matchmaker request", 3)
		}
		properties := request.GetStringProperties()
		if err := m.validateClientProfile(properties["build_hash"], properties["region"]); err != nil {
			return nil, err
		}
		// Preserve the original envelope, including group queries, extra string
		// or numeric properties, counts, and correlation ID.
		return in, nil
	}); err != nil {
		return err
	}
	if err := initializer.RegisterMatchmakerMatched(func(ctx context.Context, logger runtime.Logger, db *sql.DB, nk runtime.NakamaModule, entries []runtime.MatchmakerEntry) (string, error) {
		if len(entries) != 2 {
			return "", runtime.NewError("DM requires two players", 3)
		}
		users := make([]string, 0, 2)
		tickets := make([]string, 0, 2)
		for _, entry := range entries {
			props := entry.GetProperties()
			buildHash, _ := props["build_hash"].(string)
			region, _ := props["region"].(string)
			if err := m.validateClientProfile(buildHash, region); err != nil {
				return "", err
			}
			users = append(users, entry.GetPresence().GetUserId())
			tickets = append(tickets, entry.GetTicket())
		}
		sort.Strings(tickets)
		hash := sha256.Sum256([]byte(strings.Join(tickets, "\n")))
		_, err := m.Allocate(ctx, users, "matchmaker:"+hex.EncodeToString(hash[:]))
		if err != nil {
			return "", runtimeError(err)
		}
		return "", nil
	}); err != nil {
		return err
	}
	for _, route := range []struct {
		name   string
		resume bool
	}{{"fleet_assignment_get_v1", false}, {"fleet_resume_v1", true}} {
		resume := route.resume
		if err := initializer.RegisterRpc(route.name, func(ctx context.Context, logger runtime.Logger, db *sql.DB, nk runtime.NakamaModule, payload string) (string, error) {
			return m.assignmentRPC(ctx, payload, resume)
		}); err != nil {
			return err
		}
	}
	if err := initializer.RegisterRpc("fleet_assignment_cancel_v1", func(ctx context.Context, logger runtime.Logger, db *sql.DB, nk runtime.NakamaModule, payload string) (string, error) {
		user, _ := ctx.Value(runtime.RUNTIME_CTX_USER_ID).(string)
		if user == "" {
			return "", runtime.NewError("user authentication required", 16)
		}
		var request struct {
			AllocationID string `json:"allocation_id"`
		}
		if json.Unmarshal([]byte(payload), &request) != nil || request.AllocationID == "" {
			return "", runtime.NewError("allocation_id required", 3)
		}
		if err := m.Cancel(ctx, user, request.AllocationID); err != nil {
			return "", runtimeError(err)
		}
		return `{"accepted":true}`, nil
	}); err != nil {
		return err
	}
	return nil
}

func (m *Manager) validateClientProfile(buildHash, region string) error {
	if buildHash != m.cfg.BuildHash {
		return runtime.NewError("fleet_build_mismatch", 9)
	}
	if region != m.cfg.Region {
		return runtime.NewError("fleet_region_mismatch", 9)
	}
	return nil
}

type assignmentRequest struct {
	AllocationID string          `json:"allocation_id"`
	BuildHash    json.RawMessage `json:"build_hash"`
	Region       json.RawMessage `json:"region"`
}

func (m *Manager) assignmentRPC(ctx context.Context, payload string, resume bool) (string, error) {
	user, _ := ctx.Value(runtime.RUNTIME_CTX_USER_ID).(string)
	if user == "" {
		return "", runtime.NewError("user authentication required", 16)
	}
	var request assignmentRequest
	if payload != "" && json.Unmarshal([]byte(payload), &request) != nil {
		return "", runtime.NewError("invalid payload", 3)
	}
	// Omitted profile fields preserve older SDK requests. A provided field
	// (including an empty string or null) must match before any allocation lookup.
	for _, field := range []struct {
		payload            json.RawMessage
		expected, mismatch string
	}{
		{request.BuildHash, m.cfg.BuildHash, "fleet_build_mismatch"},
		{request.Region, m.cfg.Region, "fleet_region_mismatch"},
	} {
		if field.payload == nil {
			continue
		}
		var value *string
		if err := json.Unmarshal(field.payload, &value); err != nil {
			return "", runtime.NewError("invalid payload", 3)
		}
		if value == nil || *value != field.expected {
			return "", runtime.NewError(field.mismatch, 9)
		}
	}
	assignment, err := m.Assignment(ctx, user, request.AllocationID, resume)
	if err != nil {
		return "", runtimeError(err)
	}
	body, err := json.Marshal(assignment)
	return string(body), err
}

func runtimeError(err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return runtime.NewError("assignment not found", 5)
	case errors.Is(err, ErrForbidden):
		return runtime.NewError("forbidden", 7)
	case errors.Is(err, ErrBusy):
		return runtime.NewError("assignment unavailable", 9)
	default:
		return runtime.NewError("fleet operation failed", 13)
	}
}
