package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Postgres is the initial single-pool transactional store. A namespace row lock
// serializes admission across plugin instances. It deliberately prioritizes
// crash safety over high-scale writes; the schema is private to the fleet DB.
type Postgres struct {
	DB        *sql.DB
	Namespace string
}

func Migrate(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS fleet_state (
	 namespace TEXT PRIMARY KEY,
	 payload JSONB NOT NULL,
	 updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`)
	return err
}

func NewPostgres(ctx context.Context, db *sql.DB, namespace string) (*Postgres, error) {
	if namespace == "" {
		return nil, fmt.Errorf("fleet namespace is required")
	}
	b, _ := json.Marshal(New())
	_, err := db.ExecContext(ctx, `INSERT INTO fleet_state(namespace,payload) VALUES($1,$2) ON CONFLICT(namespace) DO NOTHING`, namespace, b)
	if err != nil {
		return nil, err
	}
	return &Postgres{DB: db, Namespace: namespace}, nil
}
func (p *Postgres) View(ctx context.Context) (*State, error) {
	var raw []byte
	if err := p.DB.QueryRowContext(ctx, `SELECT payload FROM fleet_state WHERE namespace=$1`, p.Namespace).Scan(&raw); err != nil {
		return nil, err
	}
	s := New()
	if err := json.Unmarshal(raw, s); err != nil {
		return nil, err
	}
	s.Normalize()
	return s, nil
}
func (p *Postgres) Update(ctx context.Context, fn func(*State) error) error {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT payload FROM fleet_state WHERE namespace=$1 FOR UPDATE`, p.Namespace).Scan(&raw); err != nil {
		return err
	}
	s := New()
	if err = json.Unmarshal(raw, s); err != nil {
		return err
	}
	s.Normalize()
	if err = fn(s); err != nil {
		return err
	}
	s.Revision++
	raw, err = json.Marshal(s)
	if err != nil {
		return err
	}
	if len(raw) > 16<<20 {
		return fmt.Errorf("fleet state exceeds the initial store capacity; prune terminal history or shard pools")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE fleet_state SET payload=$1, updated_at=now() WHERE namespace=$2`, raw, p.Namespace); err != nil {
		return err
	}
	return tx.Commit()
}
