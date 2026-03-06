package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/neosenth/gmail-smtp-relay/internal/gmail"
	"github.com/neosenth/gmail-smtp-relay/internal/queue"
)

type fakeQueue struct {
	msg        *queue.Message
	markSent   bool
	markRetry  bool
	markFailed bool
}

func (f *fakeQueue) Enqueue(context.Context, queue.Message) error { return nil }
func (f *fakeQueue) BacklogCount(context.Context) (int, error)    { return 0, nil }
func (f *fakeQueue) Stats(context.Context, time.Time) (queue.Stats, error) {
	return queue.Stats{}, nil
}
func (f *fakeQueue) ClaimNextPending(context.Context, time.Time) (*queue.Message, error) {
	return f.msg, nil
}
func (f *fakeQueue) MarkSent(context.Context, string, time.Time) error {
	f.markSent = true
	return nil
}
func (f *fakeQueue) MarkRetry(context.Context, string, time.Time, string, time.Time) error {
	f.markRetry = true
	return nil
}
func (f *fakeQueue) MarkFailed(context.Context, string, string, time.Time) error {
	f.markFailed = true
	return nil
}
func (f *fakeQueue) RecoverStuckProcessing(context.Context, time.Time, time.Time) (int, error) {
	return 0, nil
}
func (f *fakeQueue) Close() error { return nil }

type fakeSender struct {
	err error
}

func (f *fakeSender) Send(context.Context, string, []string, []byte) error {
	return f.err
}

func newTestRunner(t *testing.T, q queue.Store, s gmail.Sender) *Runner {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := NewRunner(Config{
		PollInterval:     time.Second,
		Concurrency:      1,
		MaxAttempts:      3,
		RetryBaseBackoff: time.Second,
		RetryMaxBackoff:  10 * time.Second,
	}, logger, q, s)
	if err != nil {
		t.Fatalf("NewRunner error: %v", err)
	}
	return r
}

func TestRunnerProcessOne_MarkSentOnSuccess(t *testing.T) {
	q := &fakeQueue{
		msg: &queue.Message{
			ID:         "m1",
			MailFrom:   "from@example.com",
			Recipients: []string{"to@example.com"},
			Data:       []byte("body"),
			Attempts:   1,
		},
	}
	r := newTestRunner(t, q, &fakeSender{})
	if err := r.processOne(context.Background()); err != nil {
		t.Fatalf("processOne error: %v", err)
	}
	if !q.markSent || q.markRetry || q.markFailed {
		t.Fatalf("unexpected mark state sent=%v retry=%v failed=%v", q.markSent, q.markRetry, q.markFailed)
	}
}

func TestRunnerProcessOne_MarkRetryOnTransient(t *testing.T) {
	q := &fakeQueue{
		msg: &queue.Message{
			ID:         "m2",
			MailFrom:   "from@example.com",
			Recipients: []string{"to@example.com"},
			Data:       []byte("body"),
			Attempts:   1,
		},
	}
	r := newTestRunner(t, q, &fakeSender{
		err: &gmail.SendError{Kind: gmail.ErrorKindTransient, Err: errors.New("temporary")},
	})
	if err := r.processOne(context.Background()); err != nil {
		t.Fatalf("processOne error: %v", err)
	}
	if !q.markRetry || q.markSent || q.markFailed {
		t.Fatalf("unexpected mark state sent=%v retry=%v failed=%v", q.markSent, q.markRetry, q.markFailed)
	}
}

func TestRunnerProcessOne_MarkFailedOnPermanent(t *testing.T) {
	q := &fakeQueue{
		msg: &queue.Message{
			ID:         "m3",
			MailFrom:   "from@example.com",
			Recipients: []string{"to@example.com"},
			Data:       []byte("body"),
			Attempts:   1,
		},
	}
	r := newTestRunner(t, q, &fakeSender{
		err: &gmail.SendError{Kind: gmail.ErrorKindPermanent, Err: errors.New("permanent")},
	})
	if err := r.processOne(context.Background()); err != nil {
		t.Fatalf("processOne error: %v", err)
	}
	if !q.markFailed || q.markSent || q.markRetry {
		t.Fatalf("unexpected mark state sent=%v retry=%v failed=%v", q.markSent, q.markRetry, q.markFailed)
	}
}
