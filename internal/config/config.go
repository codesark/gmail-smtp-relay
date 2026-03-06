package config

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type AuthUser struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type Config struct {
	SMTPBindAddr465               string
	SMTPBindAddr587               string
	SMTPRequireTLS587             bool
	SMTPMaxConnections            int
	SMTPMaxMessageBytes           int64
	SMTPMaxRecipients             int
	SMTPMaxCommandsPerSession     int
	SMTPReadTimeoutSeconds        int
	SMTPWriteTimeoutSeconds       int
	SMTPIdleTimeoutSeconds        int
	SMTPAuthRateLimitPerMin       int
	SMTPAuthLockoutSeconds        int
	SMTPHostname                  string
	QueueDBPath                   string
	QueueMaxBacklog               int
	WorkerPollIntervalSeconds     int
	WorkerConcurrency             int
	WorkerMaxAttempts             int
	WorkerRetryBaseSeconds        int
	WorkerRetryMaxSeconds         int
	SendAsRefreshSeconds          int
	ObsHTTPAddr                   string
	ReadinessAliasMaxStaleSeconds int
	ProcessingStuckTimeoutSeconds int

	AllowedSenderRegex   string
	AllowedSenderPattern *regexp.Regexp
	AuthUsers            []AuthUser

	TLSCertFile string
	TLSKeyFile  string

	GmailClientID         string
	GmailClientSecret     string
	GmailRefreshToken     string
	GmailOAuthRedirectURI string
	GmailMailbox          string

	LogLevel slog.Level
}

func (c Config) ValidateCredentials(username, password string) bool {
	for _, u := range c.AuthUsers {
		if subtle.ConstantTimeCompare([]byte(u.Username), []byte(username)) == 1 &&
			subtle.ConstantTimeCompare([]byte(u.Password), []byte(password)) == 1 {
			return true
		}
	}
	return false
}

func Load() (Config, error) {
	_ = godotenv.Load(".env")

	cfg := Config{
		SMTPBindAddr465:               getOrDefault("SMTP_BIND_ADDR_465", ":465"),
		SMTPBindAddr587:               getOrDefault("SMTP_BIND_ADDR_587", ":587"),
		SMTPRequireTLS587:             getBoolOrDefault("SMTP_REQUIRE_TLS_587", true),
		SMTPMaxConnections:            getIntOrDefault("SMTP_MAX_CONNECTIONS", 200),
		SMTPMaxMessageBytes:           getInt64OrDefault("SMTP_MAX_MESSAGE_BYTES", 25*1024*1024),
		SMTPMaxRecipients:             getIntOrDefault("SMTP_MAX_RECIPIENTS", 100),
		SMTPMaxCommandsPerSession:     getIntOrDefault("SMTP_MAX_COMMANDS_PER_SESSION", 200),
		SMTPReadTimeoutSeconds:        getIntOrDefault("SMTP_READ_TIMEOUT_SECONDS", 30),
		SMTPWriteTimeoutSeconds:       getIntOrDefault("SMTP_WRITE_TIMEOUT_SECONDS", 30),
		SMTPIdleTimeoutSeconds:        getIntOrDefault("SMTP_IDLE_TIMEOUT_SECONDS", 120),
		SMTPAuthRateLimitPerMin:       getIntOrDefault("SMTP_AUTH_RATE_LIMIT_PER_MIN", 60),
		SMTPAuthLockoutSeconds:        getIntOrDefault("SMTP_AUTH_LOCKOUT_SECONDS", 300),
		SMTPHostname:                  getOrDefault("SMTP_HOSTNAME", "gmail-smtp-relay"),
		QueueDBPath:                   getOrDefault("QUEUE_DB_PATH", "./data/queue.db"),
		QueueMaxBacklog:               getIntOrDefault("QUEUE_MAX_BACKLOG", 10000),
		WorkerPollIntervalSeconds:     getIntOrDefault("WORKER_POLL_INTERVAL_SECONDS", 1),
		WorkerConcurrency:             getIntOrDefault("WORKER_CONCURRENCY", 4),
		WorkerMaxAttempts:             getIntOrDefault("WORKER_MAX_ATTEMPTS", 8),
		WorkerRetryBaseSeconds:        getIntOrDefault("WORKER_RETRY_BASE_SECONDS", 5),
		WorkerRetryMaxSeconds:         getIntOrDefault("WORKER_RETRY_MAX_SECONDS", 300),
		SendAsRefreshSeconds:          getIntOrDefault("SENDAS_REFRESH_SECONDS", 300),
		ObsHTTPAddr:                   getOrDefault("OBS_HTTP_ADDR", "127.0.0.1:8080"),
		ReadinessAliasMaxStaleSeconds: getIntOrDefault("READINESS_ALIAS_MAX_STALE_SECONDS", 900),
		ProcessingStuckTimeoutSeconds: getIntOrDefault("PROCESSING_STUCK_TIMEOUT_SECONDS", 900),
		AllowedSenderRegex:            os.Getenv("ALLOWED_SENDER_REGEX"),
		TLSCertFile:                   os.Getenv("TLS_CERT_FILE"),
		TLSKeyFile:                    os.Getenv("TLS_KEY_FILE"),
		GmailClientID:                 os.Getenv("GMAIL_CLIENT_ID"),
		GmailClientSecret:             os.Getenv("GMAIL_CLIENT_SECRET"),
		GmailRefreshToken:             os.Getenv("GMAIL_REFRESH_TOKEN"),
		GmailOAuthRedirectURI:         os.Getenv("GMAIL_OAUTH_REDIRECT_URI"),
		GmailMailbox:                  os.Getenv("GMAIL_MAILBOX"),
		LogLevel:                      parseLogLevel(getOrDefault("LOG_LEVEL", "info")),
	}

	authUsers, err := parseAuthUsersJSON(os.Getenv("SMTP_AUTH_USERS_JSON"))
	if err != nil {
		return Config{}, err
	}
	cfg.AuthUsers = authUsers

	if cfg.AllowedSenderRegex == "" {
		return Config{}, errors.New("missing required env var ALLOWED_SENDER_REGEX")
	}
	re, err := regexp.Compile(cfg.AllowedSenderRegex)
	if err != nil {
		return Config{}, fmt.Errorf("invalid ALLOWED_SENDER_REGEX: %w", err)
	}
	cfg.AllowedSenderPattern = re

	required := map[string]string{
		"GMAIL_CLIENT_ID":     cfg.GmailClientID,
		"GMAIL_CLIENT_SECRET": cfg.GmailClientSecret,
		"GMAIL_REFRESH_TOKEN": cfg.GmailRefreshToken,
		"GMAIL_MAILBOX":       cfg.GmailMailbox,
		"TLS_CERT_FILE":       cfg.TLSCertFile,
		"TLS_KEY_FILE":        cfg.TLSKeyFile,
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			return Config{}, fmt.Errorf("missing required env var %s", name)
		}
	}

	if cfg.SMTPMaxConnections <= 0 || cfg.SMTPMaxMessageBytes <= 0 || cfg.SMTPMaxRecipients <= 0 || cfg.SMTPMaxCommandsPerSession <= 0 {
		return Config{}, errors.New("smtp limits must be positive")
	}
	if cfg.SMTPReadTimeoutSeconds <= 0 || cfg.SMTPWriteTimeoutSeconds <= 0 || cfg.SMTPIdleTimeoutSeconds <= 0 {
		return Config{}, errors.New("smtp timeout values must be positive")
	}
	if cfg.SMTPAuthRateLimitPerMin <= 0 || cfg.SMTPAuthLockoutSeconds <= 0 {
		return Config{}, errors.New("smtp auth protection values must be positive")
	}
	if strings.TrimSpace(cfg.QueueDBPath) == "" || cfg.QueueMaxBacklog <= 0 {
		return Config{}, errors.New("queue config is invalid: QUEUE_DB_PATH must be set and QUEUE_MAX_BACKLOG must be positive")
	}
	if cfg.WorkerPollIntervalSeconds <= 0 || cfg.WorkerConcurrency <= 0 || cfg.WorkerMaxAttempts <= 0 || cfg.WorkerRetryBaseSeconds <= 0 || cfg.WorkerRetryMaxSeconds <= 0 {
		return Config{}, errors.New("worker config values must be positive")
	}
	if cfg.WorkerRetryBaseSeconds > cfg.WorkerRetryMaxSeconds {
		return Config{}, errors.New("WORKER_RETRY_BASE_SECONDS must be <= WORKER_RETRY_MAX_SECONDS")
	}
	if cfg.SendAsRefreshSeconds <= 0 {
		return Config{}, errors.New("SENDAS_REFRESH_SECONDS must be positive")
	}
	if strings.TrimSpace(cfg.ObsHTTPAddr) == "" {
		return Config{}, errors.New("OBS_HTTP_ADDR must not be empty")
	}
	if cfg.ReadinessAliasMaxStaleSeconds <= 0 {
		return Config{}, errors.New("READINESS_ALIAS_MAX_STALE_SECONDS must be positive")
	}
	if cfg.ProcessingStuckTimeoutSeconds <= 0 {
		return Config{}, errors.New("PROCESSING_STUCK_TIMEOUT_SECONDS must be positive")
	}

	return cfg, nil
}

func parseAuthUsersJSON(raw string) ([]AuthUser, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("missing required env var SMTP_AUTH_USERS_JSON")
	}

	var users []AuthUser
	if err := json.Unmarshal([]byte(raw), &users); err != nil {
		return nil, fmt.Errorf("invalid SMTP_AUTH_USERS_JSON: %w", err)
	}
	if len(users) == 0 {
		return nil, errors.New("SMTP_AUTH_USERS_JSON must contain at least one user")
	}

	seen := make(map[string]struct{}, len(users))
	for _, user := range users {
		if strings.TrimSpace(user.Username) == "" || strings.TrimSpace(user.Password) == "" {
			return nil, errors.New("SMTP_AUTH_USERS_JSON contains empty username/password")
		}
		if _, ok := seen[user.Username]; ok {
			return nil, errors.New("SMTP_AUTH_USERS_JSON contains duplicate username")
		}
		seen[user.Username] = struct{}{}
	}

	return users, nil
}

func parseLogLevel(v string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func getOrDefault(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

func getBoolOrDefault(name string, fallback bool) bool {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return parsed
}

func getIntOrDefault(name string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return parsed
}

func getInt64OrDefault(name string, fallback int64) int64 {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}
