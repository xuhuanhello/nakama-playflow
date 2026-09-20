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

	"github.com/heroiclabs/nakama-common/runtime"
)

// Register installs the standard manager and game bridge. This application owns
// the single matchmaker-matched hook; existing games must compose that hook.
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
	if err := initializer.RegisterMatchmakerMatched(func(ctx context.Context, logger runtime.Logger, db *sql.DB, nk runtime.NakamaModule, entries []runtime.MatchmakerEntry) (string, error) {
		if len(entries) != 2 {
			return "", runtime.NewError("DM requires two players", 3)
		}
		users := make([]string, 0, 2)
		tickets := make([]string, 0, 2)
		for _, entry := range entries {
			props := entry.GetProperties()
			if props["build_hash"] != m.cfg.BuildHash || props["region"] != m.cfg.Region {
				return "", runtime.NewError("incompatible build or region", 9)
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
			user, _ := ctx.Value(runtime.RUNTIME_CTX_USER_ID).(string)
			if user == "" {
				return "", runtime.NewError("user authentication required", 16)
			}
			var request struct {
				AllocationID string `json:"allocation_id"`
			}
			if payload != "" && json.Unmarshal([]byte(payload), &request) != nil {
				return "", runtime.NewError("invalid payload", 3)
			}
			assignment, err := m.Assignment(ctx, user, request.AllocationID, resume)
			if err != nil {
				return "", runtimeError(err)
			}
			body, err := json.Marshal(assignment)
			return string(body), err
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
	background, cancel := context.WithCancel(context.Background())
	if err := initializer.RegisterShutdown(func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule) { cancel() }); err != nil {
		cancel()
		return err
	}
	go m.Run(background, func(err error) { logger.Error("fleet reconciliation: %v", err) })
	logger.Info("PlayFlow FleetManager registered; namespace=%s mode=%s rooms=%d", m.cfg.DeploymentID, m.cfg.Mode, m.cfg.MaxRooms)
	return nil
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
