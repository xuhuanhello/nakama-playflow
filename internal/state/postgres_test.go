package state

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// These tests require explicit opt-in to an isolated test database. Every row
// gets a random namespace; cleanup deletes only exact namespaces created by the
// current test. Tests never truncate/drop tables or inspect unrelated rows.
// Run with FLEET_TEST_DATABASE_URL set, then go test -race ./internal/state.
type postgresFixture struct {
	dsn        string
	db         *sql.DB
	namespaces []string
}

func newPostgresFixture(t *testing.T) *postgresFixture {
	t.Helper()
	dsn := os.Getenv("FLEET_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set FLEET_TEST_DATABASE_URL to an isolated PostgreSQL database to run integration tests")
	}
	fixture := &postgresFixture{dsn: dsn}
	fixture.db = fixture.open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := Migrate(ctx, fixture.db); err != nil {
		t.Fatalf("create integration-test schema: %v", err)
	}
	t.Cleanup(func() {
		// The persistence test deliberately closes the original pool. Open a
		// fresh cleanup connection so that row cleanup still always runs.
		db, err := sql.Open("pgx", fixture.dsn)
		if err != nil {
			t.Errorf("open row-cleanup connection: %v", err)
			return
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, namespace := range fixture.namespaces {
			if _, err := db.ExecContext(ctx, `DELETE FROM fleet_state WHERE namespace=$1`, namespace); err != nil {
				t.Errorf("delete test-owned namespace %q: %v", namespace, err)
			}
		}
	})
	return fixture
}

func (f *postgresFixture) open(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", f.dsn)
	if err != nil {
		t.Fatalf("open integration-test database: %v", err)
	}
	db.SetMaxOpenConns(12)
	db.SetMaxIdleConns(4)
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("connect to integration-test database: %v", err)
	}
	return db
}

func (f *postgresFixture) newStore(t *testing.T) *Postgres {
	t.Helper()
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal("generate test namespace")
	}
	namespace := "fleet-integration-" + hex.EncodeToString(random[:])
	f.namespaces = append(f.namespaces, namespace)
	return openPostgresStore(t, f.db, namespace)
}

func openPostgresStore(t *testing.T, db *sql.DB, namespace string) *Postgres {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := NewPostgres(ctx, db, namespace)
	if err != nil {
		t.Fatalf("open test namespace: %v", err)
	}
	return store
}

func viewPostgres(t *testing.T, store *Postgres) *State {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snapshot, err := store.View(ctx)
	if err != nil {
		t.Fatalf("read persisted test state: %v", err)
	}
	return snapshot
}

func TestPostgresConcurrentUpdatesIntegration(t *testing.T) {
	fixture := newPostgresFixture(t)
	first := fixture.newStore(t)
	// Separate sql.DB pools model independent Nakama plugin processes. A Go
	// mutex on a particular store object cannot make this test pass.
	second := openPostgresStore(t, fixture.open(t), first.Namespace)
	stores := []*Postgres{first, second}
	const writers, writesPerWriter = 12, 20
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := make(chan struct{})
	failures := make(chan error, writers)
	var group sync.WaitGroup
	for writer := range writers {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			<-start
			for range writesPerWriter {
				if err := stores[index%len(stores)].Update(ctx, func(s *State) error {
					s.NextCreateAt++
					return nil
				}); err != nil {
					failures <- err
					return
				}
			}
		}(writer)
	}
	close(start)
	group.Wait()
	close(failures)
	for err := range failures {
		t.Errorf("concurrent transaction: %v", err)
	}
	persisted := viewPostgres(t, first)
	if expected := int64(writers * writesPerWriter); persisted.NextCreateAt != expected || persisted.Revision != expected {
		t.Fatalf("lost updates: counter=%d revision=%d; want both %d", persisted.NextCreateAt, persisted.Revision, expected)
	}
	if other := viewPostgres(t, second); !reflect.DeepEqual(persisted, other) {
		t.Fatal("independent database pools observed different committed state")
	}
}

func TestPostgresUniqueAdmissionIntegration(t *testing.T) {
	fixture := newPostgresFixture(t)
	first := fixture.newStore(t)
	second := openPostgresStore(t, fixture.open(t), first.Namespace)
	stores := []*Postgres{first, second}
	const contenders = 16
	errAlreadyAssigned := errors.New("user already assigned")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, contenders)
	var group sync.WaitGroup
	for contender := range contenders {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			<-start
			results <- stores[index%len(stores)].Update(ctx, func(s *State) error {
				for _, allocation := range s.Allocations {
					if Terminal(allocation.State) {
						continue
					}
					for _, user := range allocation.UserIDs {
						if user == "same-player" {
							return errAlreadyAssigned
						}
					}
				}
				allocationID := fmt.Sprintf("candidate-%d", index)
				s.Allocations[allocationID] = &Allocation{ID: allocationID, State: "waiting_capacity", UserIDs: []string{"same-player", fmt.Sprintf("opponent-%d", index)}}
				return nil
			})
		}(contender)
	}
	close(start)
	group.Wait()
	close(results)
	accepted, refused := 0, 0
	for err := range results {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, errAlreadyAssigned):
			refused++
		default:
			t.Errorf("unexpected admission transaction failure: %v", err)
		}
	}
	persisted := viewPostgres(t, first)
	if accepted != 1 || refused != contenders-1 || len(persisted.Allocations) != 1 || persisted.Revision != 1 {
		t.Fatalf("admission was not serialized: accepted=%d refused=%d allocations=%d revision=%d", accepted, refused, len(persisted.Allocations), persisted.Revision)
	}
}

func TestPostgresTransactionRollbackIntegration(t *testing.T) {
	fixture := newPostgresFixture(t)
	store := fixture.newStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := store.Update(ctx, func(s *State) error {
		s.NextCreateAt = 7
		s.Workers["existing"] = &Worker{ID: "existing", State: "ready"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := viewPostgres(t, store)
	errRejected := errors.New("transaction rejected")
	err := store.Update(ctx, func(s *State) error {
		s.NextCreateAt = 999
		delete(s.Workers, "existing")
		s.Allocations["uncommitted"] = &Allocation{ID: "uncommitted", State: "active"}
		return errRejected
	})
	if !errors.Is(err, errRejected) {
		t.Fatalf("callback failure not propagated: %v", err)
	}
	if after := viewPostgres(t, store); !reflect.DeepEqual(before, after) {
		t.Fatal("failed callback changed durable state or revision")
	}

	cancelled, stop := context.WithCancel(ctx)
	defer stop()
	err = store.Update(cancelled, func(s *State) error {
		s.NextCreateAt = 1000
		stop() // Cancel after row lock/read but before UPDATE and commit.
		return nil
	})
	if err == nil {
		t.Fatal("cancelled transaction unexpectedly committed")
	}
	if after := viewPostgres(t, store); !reflect.DeepEqual(before, after) {
		t.Fatal("cancelled transaction changed durable state or revision")
	}
}

func TestPostgresNamespacesAreIndependentIntegration(t *testing.T) {
	fixture := newPostgresFixture(t)
	first, second := fixture.newStore(t), fixture.newStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := first.Update(ctx, func(s *State) error {
		s.Leader = "first-controller"
		s.NextCreateAt = 42
		s.Allocations["same-id"] = &Allocation{ID: "same-id", State: "active"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if untouched := viewPostgres(t, second); untouched.Revision != 0 || untouched.Leader != "" || len(untouched.Allocations) != 0 || untouched.NextCreateAt != 0 {
		t.Fatal("write in the first namespace contaminated the second")
	}
	if err := second.Update(ctx, func(s *State) error {
		s.Leader = "second-controller"
		s.Allocations["same-id"] = &Allocation{ID: "same-id", State: "cancelled"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if left, right := viewPostgres(t, first), viewPostgres(t, second); left.Leader != "first-controller" || right.Leader != "second-controller" || left.Allocations["same-id"].State != "active" || right.Allocations["same-id"].State != "cancelled" {
		t.Fatal("namespace keys did not isolate equal allocation IDs")
	}
}

func TestPostgresCloseReopenPersistenceIntegration(t *testing.T) {
	fixture := newPostgresFixture(t)
	first := fixture.newStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := first.Update(ctx, func(s *State) error {
		s.Profile = "persisted-profile"
		s.Leader = "controller-before-restart"
		s.LeaseUntil = 99
		s.Workers["worker"] = &Worker{ID: "worker", ProviderID: "provider-instance", BootID: "boot", State: "ready", MaxRooms: 4, LastSequence: 27}
		s.Allocations["allocation"] = &Allocation{
			ID: "allocation", WorkerID: "worker", RoomID: "room", State: "active", Epoch: 2,
			UserIDs:  []string{"player-1", "player-2"},
			Sessions: []Session{{ID: "reservation", UserID: "player-1", Connected: true, EverConnected: true}},
		}
		s.Commands["command"] = &Command{ID: "command", Type: "drain", WorkerID: "worker", Epoch: 1}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := viewPostgres(t, first)
	if err := fixture.db.Close(); err != nil {
		t.Fatalf("close original database pool: %v", err)
	}
	reopened := openPostgresStore(t, fixture.open(t), first.Namespace)
	if after := viewPostgres(t, reopened); !reflect.DeepEqual(before, after) {
		t.Fatal("opening an existing namespace reset or lost committed lifecycle state")
	}
	if err := reopened.Update(ctx, func(s *State) error { s.Leader = "controller-after-restart"; return nil }); err != nil {
		t.Fatal(err)
	}
	if after := viewPostgres(t, reopened); after.Revision != before.Revision+1 || after.Leader != "controller-after-restart" || after.Workers["worker"].ProviderID != "provider-instance" {
		t.Fatal("reopened store could not continue the persisted lifecycle")
	}
}
