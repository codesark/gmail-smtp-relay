package queue

import (
	"context"
	"time"
)

type Message struct {
	ID         string
	MailFrom   string
	Recipients []string
	Data       []byte
	Attempts   int
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type Stats struct {
	PendingCount       int
	ProcessingCount    int
	SentCount          int
	FailedCount        int
	BacklogCount       int
	OldestPendingAge   time.Duration
	OldestPendingSince time.Time
}

type Store interface {
	Enqueue(ctx context.Context, msg Message) error
	BacklogCount(ctx context.Context) (int, error)
	Stats(ctx context.Context, now time.Time) (Stats, error)
	ClaimNextPending(ctx context.Context, now time.Time) (*Message, error)
	MarkSent(ctx context.Context, id string, now time.Time) error
	MarkRetry(ctx context.Context, id string, nextAttempt time.Time, lastErr string, now time.Time) error
	MarkFailed(ctx context.Context, id string, lastErr string, now time.Time) error
	RecoverStuckProcessing(ctx context.Context, olderThan time.Time, now time.Time) (int, error)
	Close() error
}
