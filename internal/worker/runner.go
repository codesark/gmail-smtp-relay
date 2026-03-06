package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/neosenth/gmail-smtp-relay/internal/gmail"
	"github.com/neosenth/gmail-smtp-relay/internal/queue"
)

type Config struct {
	PollInterval     time.Duration
	Concurrency      int
	MaxAttempts      int
	RetryBaseBackoff time.Duration
	RetryMaxBackoff  time.Duration
}

type Runner struct {
	cfg    Config
	log    *slog.Logger
	queue  queue.Store
	sender gmail.Sender

	processedCount atomic.Uint64
	sentCount      atomic.Uint64
	retriedCount   atomic.Uint64
	failedCount    atomic.Uint64

	statsMu         sync.RWMutex
	lastProcessedAt time.Time
}

type Stats struct {
	ProcessedCount  uint64
	SentCount       uint64
	RetriedCount    uint64
	FailedCount     uint64
	LastProcessedAt time.Time
}

func NewRunner(cfg Config, logger *slog.Logger, store queue.Store, sender gmail.Sender) (*Runner, error) {
	if logger == nil || store == nil || sender == nil {
		return nil, errors.New("runner dependencies must not be nil")
	}
	if cfg.PollInterval <= 0 || cfg.Concurrency <= 0 || cfg.MaxAttempts <= 0 || cfg.RetryBaseBackoff <= 0 || cfg.RetryMaxBackoff <= 0 {
		return nil, errors.New("runner config values must be positive")
	}
	return &Runner{
		cfg:    cfg,
		log:    logger,
		queue:  store,
		sender: sender,
	}, nil
}

func (r *Runner) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(r.cfg.Concurrency)
	for i := 0; i < r.cfg.Concurrency; i++ {
		go func(workerID int) {
			defer wg.Done()
			ticker := time.NewTicker(r.cfg.PollInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := r.processOne(ctx); err != nil {
						r.log.Error("worker process loop error", "worker_id", workerID, "error", err)
					}
				}
			}
		}(i + 1)
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

func (r *Runner) processOne(ctx context.Context) error {
	now := time.Now().UTC()
	msg, err := r.queue.ClaimNextPending(ctx, now)
	if err != nil {
		return err
	}
	if msg == nil {
		return nil
	}

	sendErr := r.sender.Send(ctx, msg.MailFrom, msg.Recipients, msg.Data)
	r.recordProcessed()
	if sendErr == nil {
		r.sentCount.Add(1)
		return r.queue.MarkSent(ctx, msg.ID, time.Now().UTC())
	}

	lastErr := sendErr.Error()
	if msg.Attempts >= r.cfg.MaxAttempts || !gmail.IsTransient(sendErr) {
		r.failedCount.Add(1)
		if err := r.queue.MarkFailed(ctx, msg.ID, lastErr, time.Now().UTC()); err != nil {
			return err
		}
		r.log.Warn("message permanently failed", "message_id", msg.ID, "attempts", msg.Attempts, "error", lastErr)
		return nil
	}

	nextAttempt := time.Now().UTC().Add(backoffDuration(msg.Attempts, r.cfg.RetryBaseBackoff, r.cfg.RetryMaxBackoff))
	r.retriedCount.Add(1)
	if err := r.queue.MarkRetry(ctx, msg.ID, nextAttempt, lastErr, time.Now().UTC()); err != nil {
		return err
	}
	r.log.Warn("message scheduled for retry", "message_id", msg.ID, "attempts", msg.Attempts, "next_attempt", nextAttempt, "error", lastErr)
	return nil
}

func (r *Runner) Stats() Stats {
	r.statsMu.RLock()
	lastProcessedAt := r.lastProcessedAt
	r.statsMu.RUnlock()
	return Stats{
		ProcessedCount:  r.processedCount.Load(),
		SentCount:       r.sentCount.Load(),
		RetriedCount:    r.retriedCount.Load(),
		FailedCount:     r.failedCount.Load(),
		LastProcessedAt: lastProcessedAt,
	}
}

func (r *Runner) recordProcessed() {
	r.processedCount.Add(1)
	r.statsMu.Lock()
	r.lastProcessedAt = time.Now().UTC()
	r.statsMu.Unlock()
}

func backoffDuration(attempt int, base, max time.Duration) time.Duration {
	// attempt starts at 1 after first claim; 1=>base, 2=>2*base, 3=>4*base...
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	if d > max {
		return max
	}
	return d
}
