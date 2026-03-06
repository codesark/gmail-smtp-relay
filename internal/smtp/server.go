package smtp

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/neosenth/gmail-smtp-relay/internal/config"
	"github.com/neosenth/gmail-smtp-relay/internal/queue"
)

var errServerClosed = errors.New("smtp server closed")

type Server struct {
	cfg config.Config
	log *slog.Logger

	tlsConfig *tls.Config
	queue     queue.Store
	aliasAuth AliasAuthorizer
	connSlots chan struct{}
	connWg    sync.WaitGroup
	connMu    sync.Mutex
	conns     map[net.Conn]struct{}
	authMu    sync.Mutex
	authState map[string]authControl
	ln465     net.Listener
	ln587     net.Listener
	closeOnce sync.Once
	closedCh  chan struct{}
}

type session struct {
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer
	wt   time.Duration

	isTLS        bool
	greeted      bool
	authed       bool
	authFailures int

	mailFrom string
	rcptTo   []string
}

type authControl struct {
	WindowStart time.Time
	Attempts    int
	LockedUntil time.Time
}

type AliasAuthorizer interface {
	IsAllowedSender(ctx context.Context, mailFrom string) (bool, error)
}

func NewServer(cfg config.Config, logger *slog.Logger, store queue.Store, aliasAuth AliasAuthorizer) (*Server, error) {
	cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load tls cert/key: %w", err)
	}
	if store == nil {
		return nil, errors.New("queue store is required")
	}

	return &Server{
		cfg: cfg,
		log: logger,
		tlsConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		},
		queue:     store,
		aliasAuth: aliasAuth,
		connSlots: make(chan struct{}, cfg.SMTPMaxConnections),
		conns:     make(map[net.Conn]struct{}),
		authState: make(map[string]authControl),
		closedCh:  make(chan struct{}),
	}, nil
}

func (s *Server) Run(ctx context.Context) error {
	ln465, err := tls.Listen("tcp", s.cfg.SMTPBindAddr465, s.tlsConfig)
	if err != nil {
		return fmt.Errorf("listen 465 failed: %w", err)
	}
	s.ln465 = ln465

	ln587, err := net.Listen("tcp", s.cfg.SMTPBindAddr587)
	if err != nil {
		_ = ln465.Close()
		return fmt.Errorf("listen 587 failed: %w", err)
	}
	s.ln587 = ln587

	errCh := make(chan error, 2)
	go s.acceptLoop(ctx, s.ln465, true, errCh)
	go s.acceptLoop(ctx, s.ln587, false, errCh)

	select {
	case <-ctx.Done():
		return s.Shutdown(context.Background())
	case err := <-errCh:
		if errors.Is(err, errServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) Shutdown(ctx context.Context) error {
	var shutdownErr error
	s.closeOnce.Do(func() {
		close(s.closedCh)
		if s.ln465 != nil {
			if err := s.ln465.Close(); err != nil {
				shutdownErr = errors.Join(shutdownErr, err)
			}
		}
		if s.ln587 != nil {
			if err := s.ln587.Close(); err != nil {
				shutdownErr = errors.Join(shutdownErr, err)
			}
		}
	})

	done := make(chan struct{})
	go func() {
		s.connWg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return shutdownErr
	case <-ctx.Done():
		s.closeActiveConnections()
		return errors.Join(shutdownErr, ctx.Err())
	}
}

func (s *Server) acceptLoop(ctx context.Context, ln net.Listener, implicitTLS bool, errCh chan<- error) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.closedCh:
				errCh <- errServerClosed
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				continue
			}
			errCh <- fmt.Errorf("accept failed: %w", err)
			return
		}

		if !s.tryAcquireConnectionSlot() {
			_ = s.writeAndClose(conn, "421 too many concurrent connections\r\n")
			continue
		}
		s.connWg.Add(1)
		go func(c net.Conn) {
			defer s.connWg.Done()
			defer s.releaseConnectionSlot()
			s.handleConn(ctx, c, implicitTLS)
		}(conn)
	}
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn, implicitTLS bool) {
	defer conn.Close()
	s.trackConn(conn, true)
	defer s.trackConn(conn, false)

	sess := &session{
		conn:  conn,
		r:     bufio.NewReader(conn),
		w:     bufio.NewWriter(conn),
		wt:    time.Duration(s.cfg.SMTPWriteTimeoutSeconds) * time.Second,
		isTLS: implicitTLS,
	}

	sess.writef("220 %s ESMTP ready\r\n", s.cfg.SMTPHostname)
	if err := sess.flush(); err != nil {
		return
	}

	commandCount := 0
	for {
		select {
		case <-ctx.Done():
			_ = sess.write("421 service shutting down\r\n")
			_ = sess.flush()
			return
		default:
		}

		if err := s.setReadDeadline(sess.conn); err != nil {
			return
		}
		line, err := sess.r.ReadString('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.log.Debug("session read error", "error", err)
			}
			return
		}
		commandCount++
		if commandCount > s.cfg.SMTPMaxCommandsPerSession {
			_ = sess.write("421 too many commands\r\n")
			_ = sess.flush()
			return
		}

		line = strings.TrimSpace(line)
		if line == "" {
			_ = sess.write("500 empty command\r\n")
			_ = sess.flush()
			continue
		}

		if err := s.handleCommand(sess, line); err != nil {
			return
		}
	}
}

func (s *Server) handleCommand(sess *session, raw string) error {
	verb, args := splitCommand(raw)
	switch verb {
	case "EHLO":
		sess.greeted = true
		sess.resetEnvelope()
		sess.writef("250-%s\r\n", s.cfg.SMTPHostname)
		if !sess.isTLS {
			sess.write("250-STARTTLS\r\n")
		}
		sess.writef("250-SIZE %d\r\n", s.cfg.SMTPMaxMessageBytes)
		sess.write("250-8BITMIME\r\n")
		sess.write("250-PIPELINING\r\n")
		if sess.isTLS {
			sess.write("250-AUTH PLAIN LOGIN\r\n")
		}
		sess.write("250 HELP\r\n")
		return sess.flush()

	case "HELO":
		sess.greeted = true
		sess.resetEnvelope()
		sess.writef("250 %s\r\n", s.cfg.SMTPHostname)
		return sess.flush()

	case "STARTTLS":
		if sess.isTLS {
			sess.write("454 tls already active\r\n")
			return sess.flush()
		}
		sess.write("220 ready to start tls\r\n")
		if err := sess.flush(); err != nil {
			return err
		}
		tlsConn := tls.Server(sess.conn, s.tlsConfig)
		if err := tlsConn.Handshake(); err != nil {
			s.log.Warn("starttls handshake failed", "error", err)
			return err
		}
		sess.conn = tlsConn
		sess.r = bufio.NewReader(tlsConn)
		sess.w = bufio.NewWriter(tlsConn)
		sess.isTLS = true
		sess.greeted = false
		sess.authed = false
		sess.resetEnvelope()
		return nil

	case "AUTH":
		if !sess.isTLS && s.cfg.SMTPRequireTLS587 {
			sess.write("530 must issue STARTTLS first\r\n")
			return sess.flush()
		}
		if !sess.greeted {
			sess.write("503 send EHLO/HELO first\r\n")
			return sess.flush()
		}
		if sess.authFailures >= 3 {
			sess.write("454 too many auth failures on this connection\r\n")
			return sess.flush()
		}
		if blocked, wait := s.authBlocked("ip:"+remoteIP(sess.conn), time.Now().UTC()); blocked {
			sess.writef("454 authentication temporarily blocked, retry in %d seconds\r\n", wait)
			return sess.flush()
		}
		return s.handleAuth(sess, args)

	case "MAIL":
		if !sess.greeted {
			sess.write("503 send EHLO/HELO first\r\n")
			return sess.flush()
		}
		if !sess.authed {
			sess.write("530 authentication required\r\n")
			return sess.flush()
		}
		if sess.mailFrom != "" {
			sess.write("503 nested MAIL not allowed\r\n")
			return sess.flush()
		}
		if !strings.HasPrefix(strings.ToUpper(args), "FROM:") {
			sess.write("501 syntax: MAIL FROM:<address>\r\n")
			return sess.flush()
		}
		addr := extractAddress(args, 5) // "FROM:"
		if addr == "" || !isValidAddress(addr) {
			sess.write("553 invalid sender address\r\n")
			return sess.flush()
		}
		if !s.cfg.AllowedSenderPattern.MatchString(addr) {
			sess.write("550 sender not allowed\r\n")
			return sess.flush()
		}
		if s.aliasAuth != nil {
			checkCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			allowed, checkErr := s.aliasAuth.IsAllowedSender(checkCtx, addr)
			cancel()
			if checkErr != nil {
				s.log.Warn("send-as validation unavailable", "error", checkErr)
				sess.write("454 sender policy temporarily unavailable\r\n")
				return sess.flush()
			}
			if !allowed {
				sess.write("550 sender not allowed by mailbox alias policy\r\n")
				return sess.flush()
			}
		}
		sess.mailFrom = addr
		sess.write("250 sender ok\r\n")
		return sess.flush()

	case "RCPT":
		if sess.mailFrom == "" {
			sess.write("503 need MAIL FROM first\r\n")
			return sess.flush()
		}
		if !strings.HasPrefix(strings.ToUpper(args), "TO:") {
			sess.write("501 syntax: RCPT TO:<address>\r\n")
			return sess.flush()
		}
		if len(sess.rcptTo) >= s.cfg.SMTPMaxRecipients {
			sess.write("452 too many recipients\r\n")
			return sess.flush()
		}
		addr := extractAddress(args, 3) // "TO:"
		if addr == "" || !isValidAddress(addr) {
			sess.write("553 invalid recipient address\r\n")
			return sess.flush()
		}
		sess.rcptTo = append(sess.rcptTo, addr)
		sess.write("250 recipient ok\r\n")
		return sess.flush()

	case "DATA":
		if sess.mailFrom == "" || len(sess.rcptTo) == 0 {
			sess.write("503 need MAIL FROM and RCPT TO first\r\n")
			return sess.flush()
		}
		pendingCount, err := s.queue.BacklogCount(context.Background())
		if err != nil {
			s.log.Error("failed to read queue backlog", "error", err)
			sess.write("451 transient local error\r\n")
			return sess.flush()
		}
		if pendingCount >= s.cfg.QueueMaxBacklog {
			sess.write("421 service temporarily unavailable, queue backlog limit reached\r\n")
			return sess.flush()
		}
		sess.write("354 end data with <CR><LF>.<CR><LF>\r\n")
		if err := sess.flush(); err != nil {
			return err
		}
		payload, size, err := consumeData(sess.r, s.cfg.SMTPMaxMessageBytes)
		if err != nil {
			s.log.Warn("failed to read data", "error", err)
			sess.write("451 transient local error\r\n")
			_ = sess.flush()
			return nil
		}
		if size > s.cfg.SMTPMaxMessageBytes {
			sess.write("552 message size exceeds fixed maximum message size\r\n")
			_ = sess.flush()
			sess.resetEnvelope()
			return nil
		}

		msg := queue.Message{
			ID:         newMessageID(),
			MailFrom:   sess.mailFrom,
			Recipients: append([]string(nil), sess.rcptTo...),
			Data:       payload,
			CreatedAt:  time.Now().UTC(),
		}
		if err := s.queue.Enqueue(context.Background(), msg); err != nil {
			s.log.Error("failed to enqueue message", "error", err)
			sess.write("451 transient local error\r\n")
			_ = sess.flush()
			return nil
		}

		sess.write("250 queued for delivery\r\n")
		_ = sess.flush()
		sess.resetEnvelope()
		return nil

	case "RSET":
		sess.resetEnvelope()
		sess.write("250 reset ok\r\n")
		return sess.flush()

	case "NOOP":
		sess.write("250 ok\r\n")
		return sess.flush()

	case "QUIT":
		sess.write("221 bye\r\n")
		_ = sess.flush()
		return io.EOF

	case "VRFY", "EXPN":
		sess.write("502 command not implemented\r\n")
		return sess.flush()

	default:
		sess.write("502 command not implemented\r\n")
		return sess.flush()
	}
}

func (s *Server) handleAuth(sess *session, args string) error {
	parts := strings.Fields(args)
	if len(parts) == 0 {
		sess.write("501 syntax: AUTH mechanism [initial-response]\r\n")
		return sess.flush()
	}

	mech := strings.ToUpper(parts[0])
	switch mech {
	case "PLAIN":
		var payload string
		if len(parts) > 1 {
			payload = parts[1]
		} else {
			sess.write("334 \r\n")
			if err := sess.flush(); err != nil {
				return err
			}
			line, err := sess.r.ReadString('\n')
			if err != nil {
				return err
			}
			payload = strings.TrimSpace(line)
		}

		decoded, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			sess.write("535 authentication failed\r\n")
			return sess.flush()
		}
		segments := strings.Split(string(decoded), "\x00")
		if len(segments) < 3 {
			sess.write("535 authentication failed\r\n")
			return sess.flush()
		}
		username := segments[1]
		password := segments[2]
		if blocked, wait := s.authBlocked("user:"+username, time.Now().UTC()); blocked {
			sess.writef("454 authentication temporarily blocked, retry in %d seconds\r\n", wait)
			return sess.flush()
		}
		if !s.cfg.ValidateCredentials(username, password) {
			sess.authFailures++
			s.recordAuthFailure(username, remoteIP(sess.conn), time.Now().UTC())
			sess.write("535 authentication failed\r\n")
			return sess.flush()
		}
		s.clearAuthState("user:" + username)
		sess.authFailures = 0
		sess.authed = true
		sess.write("235 authentication successful\r\n")
		return sess.flush()

	case "LOGIN":
		sess.write("334 VXNlcm5hbWU6\r\n")
		if err := sess.flush(); err != nil {
			return err
		}
		userLine, err := sess.r.ReadString('\n')
		if err != nil {
			return err
		}
		userBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(userLine))
		if err != nil {
			sess.write("535 authentication failed\r\n")
			return sess.flush()
		}

		sess.write("334 UGFzc3dvcmQ6\r\n")
		if err := sess.flush(); err != nil {
			return err
		}
		passLine, err := sess.r.ReadString('\n')
		if err != nil {
			return err
		}
		passBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(passLine))
		if err != nil {
			sess.write("535 authentication failed\r\n")
			return sess.flush()
		}
		username := string(userBytes)
		if blocked, wait := s.authBlocked("user:"+username, time.Now().UTC()); blocked {
			sess.writef("454 authentication temporarily blocked, retry in %d seconds\r\n", wait)
			return sess.flush()
		}

		if !s.cfg.ValidateCredentials(username, string(passBytes)) {
			sess.authFailures++
			s.recordAuthFailure(username, remoteIP(sess.conn), time.Now().UTC())
			sess.write("535 authentication failed\r\n")
			return sess.flush()
		}
		s.clearAuthState("user:" + username)
		sess.authFailures = 0
		sess.authed = true
		sess.write("235 authentication successful\r\n")
		return sess.flush()

	default:
		sess.write("504 unsupported authentication mechanism\r\n")
		return sess.flush()
	}
}

func consumeData(r *bufio.Reader, maxBytes int64) ([]byte, int64, error) {
	buf := make([]byte, 0, minInt64(maxBytes, 4096))
	var total int64
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, total, err
		}
		if strings.TrimRight(line, "\r\n") == "." {
			return buf, total, nil
		}

		// SMTP dot unstuffing (RFC 5321 section 4.5.2).
		if strings.HasPrefix(line, "..") {
			line = line[1:]
		}
		total += int64(len(line))
		if total <= maxBytes {
			buf = append(buf, line...)
			continue
		}

		// Continue consuming until "." to keep stream in sync.
		for {
			next, readErr := r.ReadString('\n')
			if readErr != nil {
				return nil, total, readErr
			}
			if strings.TrimRight(next, "\r\n") == "." {
				return nil, total, nil
			}
		}
	}
}

func newMessageID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("msg-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func splitCommand(line string) (string, string) {
	parts := strings.SplitN(line, " ", 2)
	verb := strings.ToUpper(strings.TrimSpace(parts[0]))
	if len(parts) == 1 {
		return verb, ""
	}
	return verb, strings.TrimSpace(parts[1])
}

func extractAddress(args string, prefixLen int) string {
	s := strings.TrimSpace(args[prefixLen:])
	if len(s) == 0 {
		return ""
	}
	if s[0] == '<' {
		end := strings.Index(s, ">")
		if end != -1 {
			return s[1:end]
		}
	}
	// Fallback/Legacy: just take until first space
	parts := strings.Fields(s)
	if len(parts) > 0 {
		return strings.Trim(parts[0], "<>")
	}
	return ""
}

func isValidAddress(addr string) bool {
	if addr == "" {
		return false
	}
	_, err := mail.ParseAddress(addr)
	return err == nil
}

func (s *session) resetEnvelope() {
	s.mailFrom = ""
	s.rcptTo = nil
}

func (s *session) write(msg string) error {
	_ = s.conn.SetWriteDeadline(time.Now().Add(s.wt))
	_, err := s.w.WriteString(msg)
	return err
}

func (s *session) writef(format string, args ...any) error {
	_ = s.conn.SetWriteDeadline(time.Now().Add(s.wt))
	_, err := fmt.Fprintf(s.w, format, args...)
	return err
}

func (s *session) flush() error {
	_ = s.conn.SetWriteDeadline(time.Now().Add(s.wt))
	return s.w.Flush()
}

func (s *Server) setReadDeadline(conn net.Conn) error {
	return conn.SetReadDeadline(time.Now().Add(time.Duration(s.cfg.SMTPIdleTimeoutSeconds) * time.Second))
}

func (s *Server) tryAcquireConnectionSlot() bool {
	select {
	case s.connSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Server) releaseConnectionSlot() {
	select {
	case <-s.connSlots:
	default:
	}
}

func (s *Server) writeAndClose(conn net.Conn, msg string) error {
	_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_, _ = conn.Write([]byte(msg))
	return conn.Close()
}

func (s *Server) trackConn(conn net.Conn, add bool) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if add {
		s.conns[conn] = struct{}{}
		return
	}
	delete(s.conns, conn)
}

func (s *Server) closeActiveConnections() {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	for conn := range s.conns {
		_ = conn.Close()
	}
}

func (s *Server) authBlocked(key string, now time.Time) (bool, int) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	st, ok := s.authState[key]
	if !ok {
		return false, 0
	}
	if now.Before(st.LockedUntil) {
		wait := int(st.LockedUntil.Sub(now).Seconds())
		if wait < 1 {
			wait = 1
		}
		return true, wait
	}
	return false, 0
}

func (s *Server) recordAuthFailure(username, ip string, now time.Time) {
	s.recordAuthFailureKey("user:"+username, now)
	if ip != "" {
		s.recordAuthFailureKey("ip:"+ip, now)
	}
}

func (s *Server) recordAuthFailureKey(key string, now time.Time) {
	s.authMu.Lock()
	defer s.authMu.Unlock()

	st := s.authState[key]
	if now.Before(st.LockedUntil) {
		return
	}
	if st.WindowStart.IsZero() || now.Sub(st.WindowStart) >= time.Minute {
		st.WindowStart = now
		st.Attempts = 0
	}
	st.Attempts++
	if st.Attempts >= s.cfg.SMTPAuthRateLimitPerMin {
		st.LockedUntil = now.Add(time.Duration(s.cfg.SMTPAuthLockoutSeconds) * time.Second)
		st.Attempts = 0
	}
	s.authState[key] = st
}

func (s *Server) clearAuthState(key string) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	delete(s.authState, key)
}

func remoteIP(conn net.Conn) string {
	if conn == nil || conn.RemoteAddr() == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return conn.RemoteAddr().String()
	}
	return host
}
