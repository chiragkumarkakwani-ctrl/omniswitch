package store

import (
	"context"
	"testing"
	"time"
)

func TestPostgresRetentionSkipsLockedReplicaAndReleasesLock(t *testing.T) {
	st := newMigrationTestStore(t, true, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	at := time.Now().UTC()
	insertRetentionLog(t, st, RequestLog{ID: "old", Timestamp: at.Add(-31 * 24 * time.Hour)})
	conn, err := st.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock(hashtext('omniswitch_retention'))")
		_ = conn.Close()
	}()
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(hashtext('omniswitch_retention'))"); err != nil {
		t.Fatal(err)
	}
	result, err := st.SweepRetention(ctx, DefaultRetentionPolicy(), at)
	if err != nil || !result.Skipped || result.Deleted != (RetentionDeleted{}) {
		t.Fatalf("locked replica sweep=%+v, err=%v", result, err)
	}
	assertRetentionCount(t, st, "request_logs", 1)
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_unlock(hashtext('omniswitch_retention'))"); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	// A sweep must use its dedicated session, not wait on a second connection.
	st.db.SetMaxOpenConns(1)
	result, err = st.SweepRetention(ctx, DefaultRetentionPolicy(), at)
	if err != nil || result.Skipped || result.Deleted.RequestLogs != 1 {
		t.Fatalf("unlocked replica sweep=%+v, err=%v", result, err)
	}
	var locks int
	if err := st.queryRow(ctx, "SELECT COUNT(*) FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid()").Scan(&locks); err != nil || locks != 0 {
		t.Fatalf("sweep returned a locked connection to the pool: locks=%d err=%v", locks, err)
	}
}
