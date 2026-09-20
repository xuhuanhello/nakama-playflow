package fleetmanager

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/heroiclabs/nakama-common/runtime"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/xuhuanhello/nakama-playflow/internal/playflow"
	"github.com/xuhuanhello/nakama-playflow/internal/state"
)

// NewFromEnv creates a manager without installing hooks. Run the fleet DB
// migration first. The caller owns close until Register succeeds; afterward the
// manager closes its DB when Run exits during Nakama shutdown.
func NewFromEnv(ctx context.Context) (manager *Manager, close func(), err error) {
	cfg, err := FromEnv()
	if err != nil {
		return nil, nil, err
	}
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	close = func() { _ = db.Close() }
	defer func() {
		if err != nil {
			close()
		}
	}()
	setup, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	store, err := state.NewPostgres(setup, db, cfg.DeploymentID)
	if err != nil {
		return nil, close, err
	}
	provider, err := playflow.NewClient(cfg.PlayFlowURL, cfg.PlayFlowAPIKey, &http.Client{Timeout: 5 * time.Second})
	if err != nil {
		return nil, close, err
	}
	manager, err = New(cfg, store, provider)
	if err != nil {
		return nil, close, err
	}
	manager.onStop = close
	return manager, close, nil
}

// RegisterFromEnv is the public library entry point for the included two-player
// bridge. It owns the matchmaker-matched hook; compose existing game hooks first.
func RegisterFromEnv(ctx context.Context, logger runtime.Logger, db *sql.DB, nk runtime.NakamaModule, initializer runtime.Initializer) error {
	m, close, err := NewFromEnv(ctx)
	if err != nil {
		return err
	}
	if err = Register(ctx, logger, db, nk, initializer, m); err != nil {
		close()
		return err
	}
	return nil
}
