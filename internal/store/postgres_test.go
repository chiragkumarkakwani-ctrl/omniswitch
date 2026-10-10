package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestPostgresConcurrentMigrations(t *testing.T) {
	dsn := testPostgresDSN(t)
	const replicas = 6
	var wg sync.WaitGroup
	errors := make(chan error, replicas)
	for i := 0; i < replicas; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := OpenPostgres(dsn)
			if err == nil {
				err = st.Close()
			}
			errors <- err
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	st, err := OpenPostgres(dsn, WithMigrateOnStart(false))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	assertLatestSchema(t, st)
}

func TestPostgresMigrationsWaitForAdvisoryLock(t *testing.T) {
	st := newMigrationTestStore(t, true, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := st.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(hashtext('omniswitch_schema'))"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock(hashtext('omniswitch_schema'))")
	done := make(chan error, 1)
	go func() { done <- st.Migrate(ctx) }()
	select {
	case err := <-done:
		t.Fatalf("migration did not wait for advisory lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_unlock(hashtext('omniswitch_schema'))"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertLatestSchema(t, st)
}
