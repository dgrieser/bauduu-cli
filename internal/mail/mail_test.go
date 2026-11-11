package mail

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseMultipart(t *testing.T) {
	m, err := parse(bytes.NewReader(readFixture(t, "shipping_mail.eml")))
	if err != nil {
		t.Fatal(err)
	}
	if m.From != "bauduu@brickcoast.de" || m.Subject != "Dein neues LEGO Set ist unterwegs" || m.messageID != "shipping-1@brickcoast.de" {
		t.Errorf("headers = %q / %q / %q", m.From, m.Subject, m.messageID)
	}
	if m.Date.Format(time.RFC3339) != "2026-10-14T11:32:05+02:00" {
		t.Errorf("date = %v", m.Date)
	}
	for _, want := range []string{"Trackingcode: 00340434698381831612", "Versanddienstleister DHL übergeben", "\nURL: https://www.dhl.de/trackandtrace\n"} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, m.Text)
		}
	}
	if strings.Contains(m.Text, "\r") || strings.Contains(m.Text, "=\n") {
		t.Error("text not decoded")
	}
}

func TestParseHTMLLatin1(t *testing.T) {
	m, err := Parse(bytes.NewReader(readFixture(t, "shipping_mail_html.eml")))
	if err != nil {
		t.Fatal(err)
	}
	if m.Subject != "Versandbestätigung" || m.From != "bauduu@brickcoast.de" {
		t.Errorf("headers = %q / %q", m.Subject, m.From)
	}
	for _, want := range []string{"\nTrackingcode: 00340434698381831612\n", "Prüfprozess", "Geschäftsführer"} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, m.Text)
		}
	}
}

func TestHTMLTextKeepsLinks(t *testing.T) {
	got := normalizeText(HTMLText(`<p>Sendung <a href="https://example.com/t?c=1">verfolgen</a></p><table><tr><td>Trackingcode:</td><td>123</td></tr></table>`))
	if got != "Sendung verfolgen <https://example.com/t?c=1>\n\nTrackingcode: 123" {
		t.Errorf("got %q", got)
	}
}

func TestFilter(t *testing.T) {
	f := Filter{From: "Bauduu@BrickCoast.de", Since: time.Date(2026, 10, 14, 15, 0, 0, 0, time.UTC)}
	if !f.matchFrom("bauduu@brickcoast.de") || f.matchFrom("x.bauduu@brickcoast.de") {
		t.Error("exact address match")
	}
	if !(Filter{From: "brickcoast.de"}).matchFrom("bauduu@brickcoast.de") {
		t.Error("domain match")
	}
	multi := Filter{Senders: []string{"packstation@dhl.de", " noreply@dhl.de "}}
	if !multi.matchFrom("noreply@dhl.de") || !multi.matchFrom("packstation@dhl.de") || multi.matchFrom("zustellung@dhl.de") {
		t.Error("any of several senders")
	}
	if !f.matchDate(time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC)) || f.matchDate(time.Date(2026, 10, 13, 23, 0, 0, 0, time.UTC)) {
		t.Error("since is a whole day")
	}
}

func TestNormalizeConfig(t *testing.T) {
	c, err := Config{Protocol: "POP", Host: "mail.example.com", User: "u", Password: "p", Security: "starttls"}.Normalize()
	if err != nil || c.Protocol != POP3 || c.Port != 110 || c.Mailbox != "INBOX" {
		t.Errorf("pop starttls = %+v, %v", c, err)
	}
	c, _ = Config{Host: "h", User: "u", Password: "p"}.Normalize()
	if c.Protocol != IMAP || c.Port != 993 || c.Security != SecurityTLS {
		t.Errorf("defaults = %+v", c)
	}
	if _, err := (Config{Host: "h", User: "u", Password: "p", Protocol: "smtp"}).Normalize(); err == nil {
		t.Error("smtp must be refused")
	}
	if _, err := (Config{User: "u", Password: "p"}).Normalize(); err == nil {
		t.Error("missing host must be refused")
	}
}

// testTLS returns a server certificate for 127.0.0.1 and a client config
// trusting it.
func testTLS(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	srv := httptest.NewTLSServer(nil)
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return &tls.Config{Certificates: srv.TLS.Certificates}, &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}
}

type literal struct{ *bytes.Reader }

func (l literal) Size() int64 { return l.Reader.Size() }

func splitHostPort(t *testing.T, addr string) (string, int) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(port)
	return host, n
}

func TestIMAP(t *testing.T) {
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("erika", "secret")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	add := func(raw []byte, flags ...imap.Flag) {
		if _, err := user.Append("INBOX", literal{bytes.NewReader(raw)}, &imap.AppendOptions{Flags: flags}); err != nil {
			t.Fatal(err)
		}
	}
	add(readFixture(t, "shipping_mail.eml"))
	add([]byte("From: other@example.com\r\nSubject: Hi\r\nDate: Thu, 15 Oct 2026 10:00:00 +0200\r\n\r\nTrackingcode: 1\r\n"))
	add(readFixture(t, "shipping_mail_html.eml"))
	mem.AddUser(user)

	serverTLS, clientTLS := testTLS(t)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:      imap.CapSet{imap.CapIMAP4rev1: {}},
		TLSConfig: serverTLS,
		Logger:    nopLogger{},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	host, port := splitHostPort(t, ln.Addr().String())
	cfg := Config{Protocol: IMAP, Host: host, Port: port, Security: SecurityStartTLS, User: "erika", Password: "secret", TLSConfig: clientTLS, Timeout: 5 * time.Second}
	ctx := context.Background()
	if n, err := Check(ctx, cfg); err != nil || n != 3 {
		t.Fatalf("check = %d, %v", n, err)
	}
	f := Filter{From: "bauduu@brickcoast.de", Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	msgs, err := Fetch(ctx, cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || !strings.Contains(msgs[0].Text, "00340434698381831612") || msgs[1].Subject != "Versandbestätigung" {
		t.Fatalf("msgs = %+v", msgs)
	}
	// Peeking keeps the messages unread.
	if st, _ := user.Status("INBOX", &imap.StatusOptions{NumUnseen: true}); st.NumUnseen == nil || *st.NumUnseen != 3 {
		t.Errorf("unseen = %v", st.NumUnseen)
	}
	// Skipped IDs are not fetched again.
	seen := msgs[0].ID
	msgs, err = Fetch(ctx, cfg, Filter{From: f.From, Skip: func(id string) bool { return id == seen }})
	if err != nil || len(msgs) != 1 || msgs[0].ID == seen {
		t.Errorf("skip = %+v, %v", msgs, err)
	}
	cfg.Password = "wrong"
	if _, err := Check(ctx, cfg); err == nil || !strings.Contains(err.Error(), "login") {
		t.Errorf("wrong password = %v", err)
	}
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// pop3Caps selects the optional POP3 commands the fake server supports.
type pop3Caps struct{ uidl, top bool }

// fakePOP3 serves msgs (number order) over implicit TLS; it fails on DELE.
func fakePOP3(t *testing.T, serverTLS *tls.Config, msgs []string, caps pop3Caps) net.Listener {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go servePOP3(t, conn, msgs, caps)
		}
	}()
	return ln
}

func servePOP3(t *testing.T, conn net.Conn, msgs []string, caps pop3Caps) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	send := func(format string, args ...any) {
		fmt.Fprintf(w, format+"\r\n", args...)
		w.Flush()
	}
	dot := func(s string) {
		for _, l := range strings.Split(strings.TrimSuffix(strings.ReplaceAll(s, "\r\n", "\n"), "\n"), "\n") {
			if strings.HasPrefix(l, ".") {
				l = "." + l
			}
			fmt.Fprintf(w, "%s\r\n", l)
		}
		send(".")
	}
	msg := func(arg string) (string, bool) {
		n, err := strconv.Atoi(arg)
		if err != nil || n < 1 || n > len(msgs) {
			send("-ERR no such message")
			return "", false
		}
		return msgs[n-1], true
	}
	send("+OK fake POP3")
	authed := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) == 0 {
			continue
		}
		switch cmd := strings.ToUpper(f[0]); {
		case cmd == "USER":
			send("+OK")
		case cmd == "PASS":
			if len(f) > 1 && f[1] == "secret" {
				authed = true
				send("+OK")
			} else {
				send("-ERR [AUTH] invalid password")
			}
		case !authed:
			send("-ERR not authenticated")
		case cmd == "STAT":
			send("+OK %d 1000", len(msgs))
		case cmd == "UIDL" && caps.uidl:
			send("+OK")
			var b strings.Builder
			for i := range msgs {
				fmt.Fprintf(&b, "%d uid-%d\n", i+1, i+1)
			}
			dot(b.String())
		case cmd == "TOP" && caps.top && len(f) > 1:
			if m, ok := msg(f[1]); ok {
				head, _, _ := strings.Cut(m, "\r\n\r\n")
				send("+OK")
				dot(head + "\r\n")
			}
		case cmd == "RETR" && len(f) > 1:
			if m, ok := msg(f[1]); ok {
				send("+OK")
				dot(m)
			}
		case cmd == "QUIT":
			send("+OK bye")
			return
		case cmd == "DELE":
			t.Errorf("client sent DELE")
			send("-ERR")
		default:
			send("-ERR unknown command")
		}
	}
}

func TestTagSeparatesMailboxes(t *testing.T) {
	base := Config{Protocol: IMAP, Host: "mail.example.com", Port: 993, User: "erika", Mailbox: "INBOX"}
	other := []Config{base, base, base, base}
	other[0].Host = "mail.example.org"
	other[1].User = "max"
	other[2].Mailbox = "Archiv"
	other[3].Protocol = POP3
	for _, c := range other {
		if c.tag() == base.tag() {
			t.Errorf("%+v shares the tag of %+v", c, base)
		}
	}
	upper := base
	upper.Host = "MAIL.example.com"
	if upper.tag() != base.tag() {
		t.Errorf("host case changes the tag")
	}
}

func TestPOP3(t *testing.T) {
	serverTLS, clientTLS := testTLS(t)
	old := "From: bauduu@brickcoast.de\r\nSubject: Alt\r\nDate: Mon, 1 Jun 2026 10:00:00 +0200\r\n\r\nTrackingcode: 00340400000000000001\r\n"
	other := "From: other@example.com\r\nSubject: Hi\r\nDate: Thu, 15 Oct 2026 10:00:00 +0200\r\n\r\n.leading dot\r\n"
	msgs := []string{old, string(readFixture(t, "shipping_mail.eml")), other, string(readFixture(t, "shipping_mail_html.eml"))}
	for _, caps := range []pop3Caps{{uidl: true, top: true}, {uidl: false, top: true}, {uidl: true, top: false}} {
		ln := fakePOP3(t, serverTLS, msgs, caps)
		host, port := splitHostPort(t, ln.Addr().String())
		cfg := Config{Protocol: POP3, Host: host, Port: port, User: "erika", Password: "secret", TLSConfig: clientTLS, Timeout: 5 * time.Second}
		ctx := context.Background()
		if n, err := Check(ctx, cfg); err != nil || n != 4 {
			t.Fatalf("%+v check = %d, %v", caps, n, err)
		}
		got, err := Fetch(ctx, cfg, Filter{From: "bauduu@brickcoast.de", Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || !strings.Contains(got[0].Text, "Trackingcode: 00340434698381831612") || got[1].Subject != "Versandbestätigung" {
			t.Fatalf("%+v msgs = %+v", caps, got)
		}
		norm, _ := cfg.Normalize()
		wantID := norm.tag() + "uid-2"
		if !caps.uidl {
			wantID = norm.tag() + "shipping-1@brickcoast.de"
		}
		if got[0].ID != wantID {
			t.Errorf("%+v id = %q, want %q", caps, got[0].ID, wantID)
		}
		got, err = Fetch(ctx, cfg, Filter{From: "bauduu@brickcoast.de", Skip: func(id string) bool { return id == wantID }})
		if err != nil || len(got) != 2 || got[0].Subject != "Alt" {
			t.Errorf("%+v skip = %+v, %v", caps, got, err)
		}
		cfg.Password = "nope"
		if _, err := Check(ctx, cfg); err == nil || !strings.Contains(err.Error(), "invalid password") {
			t.Errorf("wrong password = %v", err)
		}
	}
}

// A server that stops answering must not hang the session, even when the
// protocol client clears the socket deadline.
func TestDialSessionLimit(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		// Accept, then say nothing until the client hangs up.
		if c, err := ln.Accept(); err == nil {
			c.Read(make([]byte, 1))
			c.Close()
		}
	}()
	host, port := splitHostPort(t, ln.Addr().String())
	cfg := Config{Host: host, Port: port, Security: SecurityNone, Timeout: 50 * time.Millisecond}
	conn, stop, err := dial(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	_ = conn.SetDeadline(time.Time{})
	start := time.Now()
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Error("read succeeded")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("session limit not enforced: read blocked %v", d)
	}
}
