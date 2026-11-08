// Package mail reads messages from a mailbox over IMAP or POP3, read-only:
// IMAP messages are fetched with BODY.PEEK and stay unread, POP3 messages are
// never deleted.
package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Protocols and security modes accepted in Config.
const (
	IMAP = "imap"
	POP3 = "pop3"

	SecurityTLS      = "tls"
	SecurityStartTLS = "starttls"
	SecurityNone     = "none"
)

// Config describes how to reach the mailbox.
type Config struct {
	Protocol string // IMAP or POP3
	Host     string
	Port     int    // 0: default for protocol and security
	Security string // SecurityTLS (default), SecurityStartTLS or SecurityNone
	User     string
	Password string
	Mailbox  string // IMAP folder, default INBOX
	Timeout  time.Duration
	// TLSConfig overrides the TLS settings, e.g. for tests.
	TLSConfig *tls.Config
}

// Normalize fills defaults and validates the settings.
func (c Config) Normalize() (Config, error) {
	c.Protocol = strings.ToLower(strings.TrimSpace(c.Protocol))
	if c.Protocol == "" {
		c.Protocol = IMAP
	}
	if c.Protocol == "pop" {
		c.Protocol = POP3
	}
	if c.Protocol != IMAP && c.Protocol != POP3 {
		return c, fmt.Errorf("unknown mail protocol %q (want imap or pop3)", c.Protocol)
	}
	c.Security = strings.ToLower(strings.TrimSpace(c.Security))
	switch c.Security {
	case "", "ssl", SecurityTLS:
		c.Security = SecurityTLS
	case "start-tls", SecurityStartTLS:
		c.Security = SecurityStartTLS
	case "plain", "insecure", SecurityNone:
		c.Security = SecurityNone
	default:
		return c, fmt.Errorf("unknown mail security %q (want tls, starttls or none)", c.Security)
	}
	c.Host = strings.TrimSpace(c.Host)
	if c.Host == "" {
		return c, errors.New("no mail server configured (run 'bauduu setup mail')")
	}
	if c.User == "" || c.Password == "" {
		return c, errors.New("mail user and password are required (run 'bauduu setup mail')")
	}
	if c.Port == 0 {
		c.Port = DefaultPort(c.Protocol, c.Security)
	}
	if c.Mailbox == "" {
		c.Mailbox = "INBOX"
	}
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
	return c, nil
}

// DefaultPort is the well-known port for protocol and security.
func DefaultPort(protocol, security string) int {
	implicit := security == SecurityTLS || security == ""
	switch {
	case protocol == POP3 && implicit:
		return 995
	case protocol == POP3:
		return 110
	case implicit:
		return 993
	default:
		return 143
	}
}

func (c Config) addr() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

func (c Config) tlsConfig() *tls.Config {
	if c.TLSConfig != nil {
		t := c.TLSConfig.Clone()
		if t.ServerName == "" {
			t.ServerName = c.Host
		}
		return t
	}
	return &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}
}

// Message is one fetched mail, reduced to what is needed to read it.
type Message struct {
	// ID is stable across runs: "<uidvalidity>/<uid>" for IMAP, the UIDL
	// for POP3 (or the Message-ID header when the server lacks UIDL).
	ID      string    `json:"id"`
	Date    time.Time `json:"date"`
	From    string    `json:"from"`
	Subject string    `json:"subject"`
	Text    string    `json:"-"`
}

// Filter selects messages. All set conditions must match.
type Filter struct {
	// From matches the sender address case-insensitively; a value without
	// "@" matches anywhere in the address (e.g. a domain).
	From string
	// Since keeps messages sent on or after that day.
	Since time.Time
	// Skip returns true for message IDs that need not be fetched again.
	Skip func(id string) bool
}

func (f Filter) matchFrom(addr string) bool {
	if f.From == "" {
		return true
	}
	addr = strings.ToLower(addr)
	want := strings.ToLower(strings.TrimSpace(f.From))
	if strings.Contains(want, "@") {
		return addr == want
	}
	return strings.Contains(addr, want)
}

func (f Filter) matchDate(d time.Time) bool {
	if f.Since.IsZero() || d.IsZero() {
		return true
	}
	y, m, day := f.Since.Date()
	return !d.Before(time.Date(y, m, day, 0, 0, 0, 0, f.Since.Location()))
}

func (f Filter) skip(id string) bool {
	return f.Skip != nil && id != "" && f.Skip(id)
}

// Fetch returns the messages matching f, oldest first.
func Fetch(ctx context.Context, cfg Config, f Filter) ([]Message, error) {
	cfg, err := cfg.Normalize()
	if err != nil {
		return nil, err
	}
	var msgs []Message
	if cfg.Protocol == POP3 {
		msgs, err = fetchPOP3(ctx, cfg, f)
	} else {
		msgs, err = fetchIMAP(ctx, cfg, f)
	}
	if err != nil {
		return nil, err
	}
	sortByDate(msgs)
	return msgs, nil
}

// Check logs in and returns the number of messages in the mailbox.
func Check(ctx context.Context, cfg Config) (int, error) {
	cfg, err := cfg.Normalize()
	if err != nil {
		return 0, err
	}
	if cfg.Protocol == POP3 {
		return checkPOP3(ctx, cfg)
	}
	return checkIMAP(ctx, cfg)
}

// dial opens the connection with implicit TLS when configured; STARTTLS is
// negotiated by the protocol code. The connection is closed when ctx ends.
func dial(ctx context.Context, cfg Config) (net.Conn, func(), error) {
	d := &net.Dialer{Timeout: cfg.Timeout}
	var conn net.Conn
	var err error
	if cfg.Security == SecurityTLS {
		td := &tls.Dialer{NetDialer: d, Config: cfg.tlsConfig()}
		conn, err = td.DialContext(ctx, "tcp", cfg.addr())
	} else {
		conn, err = d.DialContext(ctx, "tcp", cfg.addr())
	}
	if err != nil {
		return nil, nil, fmt.Errorf("connect %s: %w", cfg.addr(), err)
	}
	// A whole session must not hang a cron job forever.
	_ = conn.SetDeadline(time.Now().Add(4 * cfg.Timeout))
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	stop := func() { close(done) }
	return conn, stop, nil
}
