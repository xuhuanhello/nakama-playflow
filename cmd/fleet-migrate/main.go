package main

import (
	"context"
	"database/sql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/xuhuanhello/nakama-playflow/internal/state"
	"log"
	"os"
	"time"
)

func main() {
	dsn := os.Getenv("FLEET_DATABASE_URL")
	if dsn == "" {
		log.Fatal("FLEET_DATABASE_URL is required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatal("invalid database driver configuration")
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = state.Migrate(ctx, db); err != nil {
		log.Fatal("fleet schema migration failed: ", err)
	}
	log.Print("fleet schema ready")
}
