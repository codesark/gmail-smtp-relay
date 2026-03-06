package smtp

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/neosenth/gmail-smtp-relay/internal/config"
	"github.com/neosenth/gmail-smtp-relay/internal/queue"
)

type allowAllAliasAuth struct{}

func (allowAllAliasAuth) IsAllowedSender(context.Context, string) (bool, error) { return true, nil }

func TestIntegration_SMTPAuthAndQueueFlow(t *testing.T) {
	t.Parallel()

	testCert, testKey := writeSelfSignedCert(t)
	dbPath := filepath.Join(t.TempDir(), "queue.db")
	store, err := queue.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore error: %v", err)
	}
	defer store.Close()

	cfg := config.Config{
		SMTPBindAddr465:           "127.0.0.1:0",
		SMTPBindAddr587:           "127.0.0.1:0",
		SMTPRequireTLS587:         false,
		SMTPMaxConnections:        10,
		SMTPMaxMessageBytes:       1024 * 1024,
		SMTPMaxRecipients:         10,
		SMTPMaxCommandsPerSession: 100,
		SMTPReadTimeoutSeconds:    15,
		SMTPWriteTimeoutSeconds:   15,
		SMTPIdleTimeoutSeconds:    30,
		SMTPHostname:              "localhost",
		QueueMaxBacklog:           100,
		SMTPAuthRateLimitPerMin:   10,
		SMTPAuthLockoutSeconds:    10,
		AllowedSenderRegex:        `.+@example\.com`,
		AllowedSenderPattern:      regexp.MustCompile(`.+@example\.com`),
		AuthUsers: []config.AuthUser{
			{Username: "user", Password: "pass"},
		},
		TLSCertFile: testCert,
		TLSKeyFile:  testKey,
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := NewServer(cfg, logger, store, allowAllAliasAuth{})
	if err != nil {
		t.Fatalf("NewServer error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Run(ctx)
	}()
	waitForListener(t, func() net.Listener { return server.ln587 })

	conn, err := net.Dial("tcp", server.ln587.Addr().String())
	if err != nil {
		t.Fatalf("dial error: %v", err)
	}
	defer conn.Close()

	r := bufio.NewReader(conn)
	readLineContains(t, r, "220")
	writeCmd(t, conn, "EHLO localhost")
	readMultiline250(t, r)

	authPlain := base64.StdEncoding.EncodeToString([]byte("\x00user\x00pass"))
	writeCmd(t, conn, "AUTH PLAIN "+authPlain)
	readLineContains(t, r, "235")

	writeCmd(t, conn, "MAIL FROM:<sender@example.com>")
	readLineContains(t, r, "250")
	writeCmd(t, conn, "RCPT TO:<to@example.com>")
	readLineContains(t, r, "250")
	writeCmd(t, conn, "DATA")
	readLineContains(t, r, "354")
	writeData(t, conn, "Subject: integration\r\n\r\nhello\r\n")
	readLineContains(t, r, "250")
	writeCmd(t, conn, "QUIT")
	readLineContains(t, r, "221")

	st, err := store.Stats(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("Stats error: %v", err)
	}
	if st.PendingCount != 1 {
		t.Fatalf("expected pending count 1, got %d", st.PendingCount)
	}

	cancel()
	_ = server.Shutdown(context.Background())
	select {
	case <-time.After(2 * time.Second):
		t.Fatalf("server did not stop")
	case <-errCh:
	}
}

func TestIntegration_AUTHRequiresStartTLSOn587(t *testing.T) {
	t.Parallel()

	testCert, testKey := writeSelfSignedCert(t)
	dbPath := filepath.Join(t.TempDir(), "queue.db")
	store, err := queue.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore error: %v", err)
	}
	defer store.Close()

	cfg := config.Config{
		SMTPBindAddr465:           "127.0.0.1:0",
		SMTPBindAddr587:           "127.0.0.1:0",
		SMTPRequireTLS587:         true,
		SMTPMaxConnections:        10,
		SMTPMaxMessageBytes:       1024 * 1024,
		SMTPMaxRecipients:         10,
		SMTPMaxCommandsPerSession: 100,
		SMTPReadTimeoutSeconds:    15,
		SMTPWriteTimeoutSeconds:   15,
		SMTPIdleTimeoutSeconds:    30,
		SMTPHostname:              "localhost",
		QueueMaxBacklog:           100,
		SMTPAuthRateLimitPerMin:   10,
		SMTPAuthLockoutSeconds:    10,
		AllowedSenderRegex:        `.+@example\.com`,
		AllowedSenderPattern:      regexp.MustCompile(`.+@example\.com`),
		AuthUsers: []config.AuthUser{
			{Username: "user", Password: "pass"},
		},
		TLSCertFile: testCert,
		TLSKeyFile:  testKey,
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := NewServer(cfg, logger, store, allowAllAliasAuth{})
	if err != nil {
		t.Fatalf("NewServer error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.Run(ctx) }()
	waitForListener(t, func() net.Listener { return server.ln587 })

	conn, err := net.Dial("tcp", server.ln587.Addr().String())
	if err != nil {
		t.Fatalf("dial error: %v", err)
	}
	defer conn.Close()

	r := bufio.NewReader(conn)
	readLineContains(t, r, "220")
	writeCmd(t, conn, "EHLO localhost")
	readMultiline250(t, r)

	authPlain := base64.StdEncoding.EncodeToString([]byte("\x00user\x00pass"))
	writeCmd(t, conn, "AUTH PLAIN "+authPlain)
	readLineContains(t, r, "530")
}

func writeCmd(t *testing.T, conn net.Conn, cmd string) {
	t.Helper()
	if _, err := conn.Write([]byte(cmd + "\r\n")); err != nil {
		t.Fatalf("write cmd error: %v", err)
	}
}

func writeData(t *testing.T, conn net.Conn, data string) {
	t.Helper()
	payload := data + ".\r\n"
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("write data error: %v", err)
	}
}

func readLineContains(t *testing.T, r *bufio.Reader, want string) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read line error: %v", err)
	}
	if !strings.Contains(line, want) {
		t.Fatalf("expected line containing %q, got %q", want, line)
	}
	return line
}

func readMultiline250(t *testing.T, r *bufio.Reader) {
	t.Helper()
	for {
		line := readLineContains(t, r, "250")
		if strings.HasPrefix(line, "250 ") {
			return
		}
	}
}

func waitForListener(t *testing.T, get func() net.Listener) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ln := get(); ln != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("listener did not become ready")
}

func writeSelfSignedCert(t *testing.T) (certPath string, keyPath string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}

	baseDir := t.TempDir()
	certOut := filepath.Join(baseDir, "cert.pem")
	keyOut := filepath.Join(baseDir, "key.pem")
	cf, err := os.Create(certOut)
	if err != nil {
		t.Fatalf("create cert file: %v", err)
	}
	defer cf.Close()
	if err := pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("encode cert: %v", err)
	}

	kf, err := os.Create(keyOut)
	if err != nil {
		t.Fatalf("create key file: %v", err)
	}
	defer kf.Close()
	if err := pem.Encode(kf, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)}); err != nil {
		t.Fatalf("encode key: %v", err)
	}
	return certOut, keyOut
}
