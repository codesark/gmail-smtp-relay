package queue

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteStore_EnqueueClaimRetryRecover(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "queue.db")
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore error: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	msg := Message{
		ID:         "msg-1",
		MailFrom:   "from@example.com",
		Recipients: []string{"to@example.com"},
		Data:       []byte("Subject: Test\r\n\r\nHello\r\n"),
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := store.Enqueue(context.Background(), msg); err != nil {
		t.Fatalf("Enqueue error: %v", err)
	}

	backlog, err := store.BacklogCount(context.Background())
	if err != nil {
		t.Fatalf("BacklogCount error: %v", err)
	}
	if backlog != 1 {
		t.Fatalf("unexpected backlog: want=1 got=%d", backlog)
	}

	claimed, err := store.ClaimNextPending(context.Background(), now.Add(1*time.Second))
	if err != nil {
		t.Fatalf("ClaimNextPending error: %v", err)
	}
	if claimed == nil || claimed.ID != "msg-1" {
		t.Fatalf("unexpected claimed message: %#v", claimed)
	}
	if claimed.Attempts != 1 {
		t.Fatalf("expected attempts=1 after claim, got %d", claimed.Attempts)
	}

	recovered, err := store.RecoverStuckProcessing(context.Background(), time.Now().UTC().Add(1*time.Second), time.Now().UTC())
	if err != nil {
		t.Fatalf("RecoverStuckProcessing error: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("expected one recovered row, got %d", recovered)
	}

	claimedAgain, err := store.ClaimNextPending(context.Background(), time.Now().UTC().Add(2*time.Second))
	if err != nil {
		t.Fatalf("ClaimNextPending second error: %v", err)
	}
	if claimedAgain == nil || claimedAgain.ID != "msg-1" {
		t.Fatalf("expected message after recovery, got %#v", claimedAgain)
	}

	if err := store.MarkSent(context.Background(), "msg-1", time.Now().UTC()); err != nil {
		t.Fatalf("MarkSent error: %v", err)
	}

	st, err := store.Stats(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("Stats error: %v", err)
	}
	if st.SentCount != 1 {
		t.Fatalf("expected sent_count=1 got=%d", st.SentCount)
	}
	if st.BacklogCount != 0 {
		t.Fatalf("expected backlog=0 got=%d", st.BacklogCount)
	}
}
