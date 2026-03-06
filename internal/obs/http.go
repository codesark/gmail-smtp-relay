package obs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/neosenth/gmail-smtp-relay/internal/queue"
	"github.com/neosenth/gmail-smtp-relay/internal/worker"
)

type AliasStatusProvider interface {
	AliasStatus() (count int, lastRefresh time.Time, lastErr string)
}

type Config struct {
	Addr             string
	AliasMaxStaleFor time.Duration
}

type Server struct {
	cfg          Config
	log          *slog.Logger
	queue        queue.Store
	worker       *worker.Runner
	aliasStatus  AliasStatusProvider
	startedAtUTC time.Time
	httpServer   *http.Server
}

func NewServer(cfg Config, logger *slog.Logger, q queue.Store, w *worker.Runner, alias AliasStatusProvider) (*Server, error) {
	if logger == nil || q == nil || w == nil || alias == nil {
		return nil, errors.New("observability dependencies must not be nil")
	}
	if cfg.Addr == "" {
		return nil, errors.New("observability addr must not be empty")
	}
	if cfg.AliasMaxStaleFor <= 0 {
		return nil, errors.New("observability alias max stale duration must be positive")
	}

	s := &Server{
		cfg:          cfg,
		log:          logger,
		queue:        q,
		worker:       w,
		aliasStatus:  alias,
		startedAtUTC: time.Now().UTC(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/metrics", s.handleMetrics)

	s.httpServer = &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s, nil
}

func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("observability http server started", "addr", s.cfg.Addr)
		errCh <- s.httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return s.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"service":    "gmail-smtp-relay",
		"started_at": s.startedAtUTC.Format(time.RFC3339Nano),
	})
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	queueStats, err := s.queue.Stats(ctx, time.Now().UTC())
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":    false,
			"error": fmt.Sprintf("queue unavailable: %v", err),
		})
		return
	}

	aliasCount, lastAliasRefresh, aliasErr := s.aliasStatus.AliasStatus()
	if aliasCount == 0 {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":    false,
			"error": "send-as alias cache is empty",
		})
		return
	}
	if aliasErr != "" && time.Since(lastAliasRefresh) > s.cfg.AliasMaxStaleFor {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":    false,
			"error": fmt.Sprintf("send-as alias cache stale: %s", aliasErr),
		})
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok":                 true,
		"queue_backlog":      queueStats.BacklogCount,
		"alias_count":        aliasCount,
		"last_alias_refresh": lastAliasRefresh.Format(time.RFC3339Nano),
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	queueStats, _ := s.queue.Stats(ctx, time.Now().UTC())
	aliasCount, lastAliasRefresh, aliasErr := s.aliasStatus.AliasStatus()
	ws := s.worker.Stats()

	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok":                 true,
		"queue_backlog":      queueStats.BacklogCount,
		"queue_pending":      queueStats.PendingCount,
		"queue_processing":   queueStats.ProcessingCount,
		"queue_sent":         queueStats.SentCount,
		"queue_failed":       queueStats.FailedCount,
		"oldest_pending_age": queueStats.OldestPendingAge.String(),
		"alias_count":        aliasCount,
		"last_alias_refresh": lastAliasRefresh.Format(time.RFC3339Nano),
		"alias_last_error":   aliasErr,
		"worker": map[string]any{
			"processed_count":   ws.ProcessedCount,
			"sent_count":        ws.SentCount,
			"retried_count":     ws.RetriedCount,
			"failed_count":      ws.FailedCount,
			"last_processed_at": ws.LastProcessedAt.Format(time.RFC3339Nano),
		},
	})
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, payload map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	queueStats, err := s.queue.Stats(ctx, time.Now().UTC())
	if err != nil {
		http.Error(w, "queue metrics unavailable", http.StatusServiceUnavailable)
		return
	}
	aliasCount, lastAliasRefresh, aliasErr := s.aliasStatus.AliasStatus()
	ws := s.worker.Stats()

	var b strings.Builder
	writeGauge := func(name string, value string, help string) {
		b.WriteString("# HELP ")
		b.WriteString(name)
		b.WriteString(" ")
		b.WriteString(help)
		b.WriteString("\n# TYPE ")
		b.WriteString(name)
		b.WriteString(" gauge\n")
		b.WriteString(name)
		b.WriteString(" ")
		b.WriteString(value)
		b.WriteString("\n")
	}
	writeCounter := func(name string, value uint64, help string) {
		b.WriteString("# HELP ")
		b.WriteString(name)
		b.WriteString(" ")
		b.WriteString(help)
		b.WriteString("\n# TYPE ")
		b.WriteString(name)
		b.WriteString(" counter\n")
		b.WriteString(name)
		b.WriteString(" ")
		b.WriteString(strconv.FormatUint(value, 10))
		b.WriteString("\n")
	}

	writeGauge("relay_queue_backlog", strconv.Itoa(queueStats.BacklogCount), "Current queue backlog size.")
	writeGauge("relay_queue_pending", strconv.Itoa(queueStats.PendingCount), "Current pending queue messages.")
	writeGauge("relay_queue_processing", strconv.Itoa(queueStats.ProcessingCount), "Current processing queue messages.")
	writeGauge("relay_queue_sent", strconv.Itoa(queueStats.SentCount), "Current sent queue messages.")
	writeGauge("relay_queue_failed", strconv.Itoa(queueStats.FailedCount), "Current failed queue messages.")
	writeGauge("relay_queue_oldest_pending_age_seconds", strconv.FormatInt(int64(queueStats.OldestPendingAge.Seconds()), 10), "Age in seconds of the oldest pending queue message.")

	writeCounter("relay_worker_processed_total", ws.ProcessedCount, "Total worker processed attempts.")
	writeCounter("relay_worker_sent_total", ws.SentCount, "Total successfully sent messages.")
	writeCounter("relay_worker_retried_total", ws.RetriedCount, "Total retried messages.")
	writeCounter("relay_worker_failed_total", ws.FailedCount, "Total permanently failed messages.")

	writeGauge("relay_alias_count", strconv.Itoa(aliasCount), "Number of cached allowed sender aliases.")
	writeGauge("relay_alias_last_refresh_unix", strconv.FormatInt(lastAliasRefresh.Unix(), 10), "Unix timestamp of last alias refresh.")
	if aliasErr == "" {
		writeGauge("relay_alias_error", "0", "Whether alias refresh currently has an error (1=yes, 0=no).")
	} else {
		writeGauge("relay_alias_error", "1", "Whether alias refresh currently has an error (1=yes, 0=no).")
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}
