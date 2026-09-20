package main

import (
	"context"
	"database/sql"

	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/xuhuanhello/nakama-playflow/pkg/fleetmanager"
)

func InitModule(ctx context.Context, logger runtime.Logger, nakamaDB *sql.DB, nk runtime.NakamaModule, initializer runtime.Initializer) error {
	return fleetmanager.RegisterFromEnv(ctx, logger, nakamaDB, nk, initializer)
}
func main() {}
