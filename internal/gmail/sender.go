package gmail

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

type ErrorKind string

const (
	ErrorKindTransient ErrorKind = "transient"
	ErrorKindPermanent ErrorKind = "permanent"
)

type SendError struct {
	Kind ErrorKind
	Err  error
}

func (e *SendError) Error() string {
	if e == nil || e.Err == nil {
		return "send error"
	}
	return e.Err.Error()
}

func (e *SendError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func IsTransient(err error) bool {
	var sendErr *SendError
	if errors.As(err, &sendErr) {
		return sendErr.Kind == ErrorKindTransient
	}
	return false
}

type Sender interface {
	Send(ctx context.Context, mailFrom string, recipients []string, rfc822 []byte) error
}

type Config struct {
	ClientID     string
	ClientSecret string
	RefreshToken string
	Mailbox      string
}

type APISender struct {
	svc             *gmail.Service
	mailbox         string
	refreshInterval time.Duration

	mu          sync.RWMutex
	allowedSend map[string]struct{}
	lastRefresh time.Time
	lastErr     string
}

func NewAPISender(ctx context.Context, cfg Config, refreshInterval time.Duration) (*APISender, error) {
	if strings.TrimSpace(cfg.ClientID) == "" ||
		strings.TrimSpace(cfg.ClientSecret) == "" ||
		strings.TrimSpace(cfg.RefreshToken) == "" ||
		strings.TrimSpace(cfg.Mailbox) == "" {
		return nil, errors.New("gmail sender config is incomplete")
	}
	if refreshInterval <= 0 {
		return nil, errors.New("gmail alias refresh interval must be positive")
	}

	oauthCfg := &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       []string{gmail.GmailSendScope, gmail.GmailSettingsBasicScope},
	}
	tokenSource := oauthCfg.TokenSource(ctx, &oauth2.Token{RefreshToken: cfg.RefreshToken})

	httpClient := &http.Client{
		Transport: &oauth2.Transport{
			Source: tokenSource,
			Base:   http.DefaultTransport,
		},
		Timeout: 30 * time.Second,
	}

	svc, err := gmail.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("create gmail service: %w", err)
	}

	sender := &APISender{
		svc:             svc,
		mailbox:         cfg.Mailbox,
		refreshInterval: refreshInterval,
		allowedSend:     make(map[string]struct{}),
	}
	if err := sender.refreshAliases(ctx); err != nil {
		return nil, fmt.Errorf("initial send-as refresh failed: %w", err)
	}
	return sender, nil
}

func (s *APISender) Send(ctx context.Context, mailFrom string, recipients []string, rfc822 []byte) error {
	if len(recipients) == 0 {
		return &SendError{Kind: ErrorKindPermanent, Err: errors.New("message has no recipients")}
	}

	allowed, err := s.IsAllowedSender(ctx, mailFrom)
	if err != nil {
		return &SendError{Kind: ErrorKindTransient, Err: fmt.Errorf("send-as validation unavailable: %w", err)}
	}
	if !allowed {
		return &SendError{Kind: ErrorKindPermanent, Err: fmt.Errorf("mail from %q is not allowed by send-as policy", mailFrom)}
	}

	raw := base64.RawURLEncoding.EncodeToString(rfc822)
	msg := &gmail.Message{Raw: raw}
	_, err = s.svc.Users.Messages.Send("me", msg).Context(ctx).Do()
	if err != nil {
		return classifyGoogleErr(err)
	}
	return nil
}

func classifyGoogleErr(err error) error {
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return &SendError{Kind: ErrorKindTransient, Err: err}
		default:
			return &SendError{Kind: ErrorKindPermanent, Err: err}
		}
	}
	return &SendError{Kind: ErrorKindTransient, Err: err}
}

func (s *APISender) RunAliasRefresh(ctx context.Context) {
	ticker := time.NewTicker(s.refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refreshCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			_ = s.refreshAliases(refreshCtx)
			cancel()
		}
	}
}

func (s *APISender) IsAllowedSender(ctx context.Context, mailFrom string) (bool, error) {
	addr := canonicalAddr(mailFrom)
	if addr == "" {
		return false, nil
	}

	s.mu.RLock()
	_, ok := s.allowedSend[strings.ToLower(addr)]
	lastRefresh := s.lastRefresh
	refreshInterval := s.refreshInterval
	s.mu.RUnlock()
	if ok {
		return true, nil
	}

	if time.Since(lastRefresh) > refreshInterval {
		if err := s.refreshAliases(ctx); err != nil {
			return false, err
		}
		s.mu.RLock()
		_, ok = s.allowedSend[strings.ToLower(addr)]
		s.mu.RUnlock()
	}
	return ok, nil
}

func (s *APISender) refreshAliases(ctx context.Context) error {
	call := s.svc.Users.Settings.SendAs.List("me").Context(ctx)
	resp, err := call.Do()
	if err != nil {
		s.setAliasError(err)
		return err
	}

	allowed := make(map[string]struct{}, len(resp.SendAs)+1)
	allowed[strings.ToLower(canonicalAddr(s.mailbox))] = struct{}{}
	for _, entry := range resp.SendAs {
		addr := canonicalAddr(entry.SendAsEmail)
		if addr == "" {
			continue
		}
		allowed[strings.ToLower(addr)] = struct{}{}
	}
	if len(allowed) == 0 {
		err := errors.New("gmail send-as list is empty")
		s.setAliasError(err)
		return err
	}

	s.mu.Lock()
	s.allowedSend = allowed
	s.lastRefresh = time.Now().UTC()
	s.lastErr = ""
	s.mu.Unlock()
	return nil
}

func (s *APISender) AliasStatus() (int, time.Time, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.allowedSend), s.lastRefresh, s.lastErr
}

func (s *APISender) setAliasError(err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	s.mu.Lock()
	s.lastErr = msg
	s.mu.Unlock()
}

func canonicalAddr(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if addr, err := mail.ParseAddress(v); err == nil {
		return strings.TrimSpace(addr.Address)
	}
	return strings.Trim(v, "<>")
}
