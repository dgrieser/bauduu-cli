package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
)

// pop3 is a minimal RFC 1939 client: USER/PASS, STAT, UIDL, TOP, RETR, QUIT.
// It never sends DELE, so the mailbox stays as it is.
type pop3 struct {
	tp   *textproto.Conn
	conn net.Conn
}

// errPOP3 is a -ERR answer.
type errPOP3 string

func (e errPOP3) Error() string { return "POP3: " + string(e) }

func (p *pop3) cmd(format string, args ...any) (string, error) {
	if err := p.tp.PrintfLine(format, args...); err != nil {
		return "", err
	}
	return p.reply()
}

func (p *pop3) reply() (string, error) {
	line, err := p.tp.ReadLine()
	if err != nil {
		return "", err
	}
	switch {
	case strings.HasPrefix(line, "+OK"):
		return strings.TrimSpace(strings.TrimPrefix(line, "+OK")), nil
	case strings.HasPrefix(line, "-ERR"):
		return "", errPOP3(strings.TrimSpace(strings.TrimPrefix(line, "-ERR")))
	default:
		return "", fmt.Errorf("POP3: unexpected reply %q", line)
	}
}

// lines reads a dot-terminated multi-line answer.
func (p *pop3) lines() ([]string, error) {
	return p.tp.ReadDotLines()
}

func pop3Connect(ctx context.Context, cfg Config) (*pop3, func(), error) {
	conn, stop, err := dial(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	p := &pop3{tp: textproto.NewConn(conn), conn: conn}
	fail := func(err error) (*pop3, func(), error) {
		stop()
		conn.Close()
		return nil, nil, err
	}
	if _, err := p.reply(); err != nil {
		return fail(fmt.Errorf("POP3 greeting: %w", err))
	}
	if cfg.Security == SecurityStartTLS {
		if _, err := p.cmd("STLS"); err != nil {
			return fail(fmt.Errorf("POP3 STLS: %w", err))
		}
		tc := tls.Client(conn, cfg.tlsConfig())
		if err := tc.HandshakeContext(ctx); err != nil {
			return fail(fmt.Errorf("POP3 TLS: %w", err))
		}
		conn = tc
		p = &pop3{tp: textproto.NewConn(tc), conn: tc}
	}
	if _, err := p.cmd("USER %s", cfg.User); err != nil {
		return fail(fmt.Errorf("POP3 login as %s: %w", cfg.User, err))
	}
	if _, err := p.cmd("PASS %s", cfg.Password); err != nil {
		return fail(fmt.Errorf("POP3 login as %s: %w", cfg.User, err))
	}
	cleanup := func() {
		_, _ = p.cmd("QUIT")
		p.tp.Close()
		stop()
	}
	return p, cleanup, nil
}

func (p *pop3) stat() (int, error) {
	r, err := p.cmd("STAT")
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.Fields(r + " x")[0])
	if err != nil {
		return 0, fmt.Errorf("POP3: bad STAT reply %q", r)
	}
	return n, nil
}

// uidl maps message numbers to unique IDs; ok is false without UIDL support.
func (p *pop3) uidl() (ids map[int]string, ok bool, err error) {
	if _, err := p.cmd("UIDL"); err != nil {
		var e errPOP3
		if errors.As(err, &e) {
			return nil, false, nil
		}
		return nil, false, err
	}
	lines, err := p.lines()
	if err != nil {
		return nil, false, err
	}
	ids = make(map[int]string, len(lines))
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		if n, err := strconv.Atoi(f[0]); err == nil {
			ids[n] = f[1]
		}
	}
	return ids, true, nil
}

func (p *pop3) body(format string, n int) (string, error) {
	if _, err := p.cmd(format, n); err != nil {
		return "", err
	}
	b, err := io.ReadAll(p.tp.DotReader())
	return string(b), err
}

func checkPOP3(ctx context.Context, cfg Config) (int, error) {
	p, cleanup, err := pop3Connect(ctx, cfg)
	if err != nil {
		return 0, err
	}
	defer cleanup()
	return p.stat()
}

// olderInARow ends the scan from newest to oldest once that many messages
// in a row are older than Filter.Since; mailboxes are mostly in order.
const olderInARow = 50

func fetchPOP3(ctx context.Context, cfg Config, f Filter) ([]Message, error) {
	p, cleanup, err := pop3Connect(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	count, err := p.stat()
	if err != nil {
		return nil, err
	}
	ids, hasUIDL, err := p.uidl()
	if err != nil {
		return nil, err
	}
	var msgs []Message
	older := 0
	hasTOP := true
	for n := count; n >= 1 && older < olderInARow; n-- {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := ids[n]
		if hasUIDL && f.skip(id) {
			older = 0
			continue
		}
		// Headers first, so only matching messages are downloaded whole.
		// TOP is optional (RFC 1939); without it every message is retrieved.
		var head, raw string
		if hasTOP {
			head, err = p.body("TOP %d 0", n)
			var e errPOP3
			if errors.As(err, &e) {
				hasTOP = false
			} else if err != nil {
				return nil, fmt.Errorf("POP3 TOP %d: %w", n, err)
			}
		}
		if !hasTOP {
			if raw, err = p.body("RETR %d", n); err != nil {
				return nil, fmt.Errorf("POP3 RETR %d: %w", n, err)
			}
			head = raw
		}
		hm, err := parseHeader(strings.NewReader(head + "\r\n"))
		if err != nil {
			continue
		}
		if !f.matchDate(hm.Date) {
			older++
			continue
		}
		older = 0
		if !f.matchFrom(hm.From) {
			continue
		}
		if !hasUIDL {
			id = hm.messageID
			if f.skip(id) {
				continue
			}
		}
		if raw == "" {
			if raw, err = p.body("RETR %d", n); err != nil {
				return nil, fmt.Errorf("POP3 RETR %d: %w", n, err)
			}
		}
		m, err := parse(strings.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("POP3 message %d: %w", n, err)
		}
		m.ID = id
		msgs = append(msgs, m.Message)
	}
	return msgs, nil
}
