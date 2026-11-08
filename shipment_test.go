package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/dgrieser/bauduu-cli/internal/bauduu"
	"github.com/dgrieser/bauduu-cli/internal/dhl"
	"github.com/dgrieser/bauduu-cli/internal/mail"
)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

func notice(id, date, code string) noticeMail {
	m := noticeMail{Message: mail.Message{ID: id, Date: at(date)}}
	if code != "" {
		m.Notice = &bauduu.ShippingNotice{Carrier: "DHL", Code: code}
	}
	return m
}

func TestAssignNotices(t *testing.T) {
	loans := []bauduu.Loan{
		{ID: 9, SKU: "42100-1", Name: "Bagger 42100", Shipped: day("2026-11-02"), Status: "Ausgeliehen"},
		{ID: 8, SKU: "31119-1", Name: "Riesenrad 3in1 31119", Shipped: day("2026-10-14"), Status: "Ausgeliehen"},
		{ID: 7, SKU: "60036-1", Name: "Arktis 60036", Shipped: day("2026-06-01"), Returned: day("2026-07-01"), Status: "Zurückgegeben"},
	}
	known := []trackedShipment{{LoanID: 1, Code: "KNOWN"}}
	mails := []noticeMail{
		notice("m0", "2026-06-01 10:00", "OLD"),
		notice("m1", "2026-10-14 11:32", "00340434698381831612"),
		notice("m2", "2026-10-20 09:00", ""),
		notice("m3", "2026-11-03 08:00", "00340400000000000002"),
		notice("m4", "2026-11-02 18:00", "KNOWN"),
	}
	pending := pendingLoans(loans, known)
	if len(pending) != 2 {
		t.Fatalf("pending = %+v", pending)
	}
	if got := mailSince(pending, time.Now()); !got.Equal(day("2026-10-09").Time) {
		t.Errorf("since = %v", got)
	}
	// An undated loan does not cut off an older one.
	now := day("2026-11-08").Time
	undated := []bauduu.Loan{{ID: 10, Status: "Ausgeliehen"}, pending[1]}
	if got := mailSince(undated, now); !got.Equal(day("2026-10-09").Time) {
		t.Errorf("since with undated loan = %v", got)
	}
	if got := mailSince(undated[:1], now); !got.Equal(now.AddDate(0, 0, -30)) {
		t.Errorf("since undated only = %v", got)
	}
	got := assignNotices(pending, known, mails)
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got[0].LoanID != 9 || got[0].Code != "00340400000000000002" || got[0].MailID != "m3" || got[0].Set != "Bagger (42100)" {
		t.Errorf("Bagger = %+v", got[0])
	}
	if got[1].LoanID != 8 || got[1].Code != "00340434698381831612" {
		t.Errorf("Riesenrad = %+v", got[1])
	}
	// A mail far from every shipping date is not assigned.
	if got := assignNotices(pending[:1], nil, mails[:2]); len(got) != 0 {
		t.Errorf("distant mail assigned: %+v", got)
	}
}

// fakeTracking serves the DHL endpoints with whatever state is set.
type fakeTracking struct {
	mu      sync.Mutex
	status  string
	events  []string
	done    bool
	queries int
}

func (f *fakeTracking) set(status string, done bool, events ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.done, f.events = status, done, events
}

func (f *fakeTracking) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/int-verfolgen/data/config":
		io.WriteString(w, `{"verfolgenCsrfToken":"t"}`)
	case "/int-verfolgen/data/search", "/int-verfolgen/data/shipment":
		f.queries++
		var ev []string
		for i, e := range f.events {
			ev = append(ev, fmt.Sprintf(`{"datum":"2026-11-0%dT10:00:00.000+01:00","ort":"Dortmund","status":%q}`, i+1, e))
		}
		fmt.Fprintf(w, `{"sendungen":[{"id":"C1","hasCompleteDetails":true,"sendungsdetails":{"istZugestellt":%t,
			"sendungsverlauf":{"status":%q,"fortschritt":2,"maximalFortschritt":5,"events":[%s]}}}]}`,
			f.done, f.status, strings.Join(ev, ","))
	default:
		http.NotFound(w, r)
	}
}

func TestUpdateShipmentsTracksUntilDelivered(t *testing.T) {
	fake := &fakeTracking{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	client, err := dhl.New(srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var warnings []error
	env := &trackEnv{dhl: client, warn: func(err error) { warnings = append(warnings, err) }}
	loans := []bauduu.Loan{{ID: 8, SKU: "31119-1", Name: "Riesenrad 3in1 31119", Shipped: day("2026-11-01"), Status: "Ausgeliehen"}}
	prev := []trackedShipment{{LoanID: 8, Set: "Riesenrad 3in1 (31119)", Carrier: "DHL", Code: "C1"}}
	now := time.Now()
	ctx := context.Background()

	fake.set("Die Sendung wurde elektronisch angekündigt.", false, "Die Sendung wurde elektronisch angekündigt.")
	u := updateShipments(ctx, env, loans, prev, nil, now)
	if len(u.Changes) != 1 || u.Changes[0].Title != "Sendung Riesenrad 3in1: Die Sendung wurde elektronisch angekündigt." || !u.Changes[0].Shipment {
		t.Fatalf("first = %+v", u.Changes)
	}
	// Nothing new: nothing reported.
	u = updateShipments(ctx, env, loans, u.Shipments, nil, now)
	if len(u.Changes) != 0 {
		t.Errorf("unchanged = %+v", u.Changes)
	}
	// Two new events: both listed in one change.
	fake.set("In Zustellung", false, "Die Sendung wurde elektronisch angekündigt.", "Im Paketzentrum bearbeitet", "In Zustellung")
	u = updateShipments(ctx, env, loans, u.Shipments, nil, now)
	cet := time.FixedZone("CET", 3600)
	want := fmt.Sprintf("Im Paketzentrum bearbeitet (Dortmund, %s); In Zustellung (Dortmund, %s)",
		time.Date(2026, 11, 2, 10, 0, 0, 0, cet).Local().Format("02.01. 15:04"),
		time.Date(2026, 11, 3, 10, 0, 0, 0, cet).Local().Format("02.01. 15:04"))
	if len(u.Changes) != 1 || !strings.Contains(u.Changes[0].Line, want) {
		t.Fatalf("events = %+v", u.Changes)
	}
	fake.set("Zugestellt", true, "Die Sendung wurde elektronisch angekündigt.", "Im Paketzentrum bearbeitet", "In Zustellung", "Zugestellt")
	u = updateShipments(ctx, env, loans, u.Shipments, nil, now)
	if len(u.Changes) != 1 || u.Changes[0].Title != "Zugestellt: Riesenrad 3in1" {
		t.Fatalf("delivered = %+v", u.Changes)
	}
	// Delivered parcels are not queried again.
	before := fake.queries
	u = updateShipments(ctx, env, loans, u.Shipments, nil, now)
	if fake.queries != before || len(u.Changes) != 0 || len(u.Shipments) != 1 {
		t.Errorf("after delivery: queries %d → %d, %+v", before, fake.queries, u)
	}
	// Once the set is back, its shipment is dropped.
	loans[0].Returned = day("2026-12-01")
	if u = updateShipments(ctx, env, loans, u.Shipments, nil, now); len(u.Shipments) != 0 {
		t.Errorf("returned set still tracked: %+v", u.Shipments)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings: %v", warnings)
	}
}

type literal struct{ *bytes.Reader }

func (l literal) Size() int64 { return l.Reader.Size() }

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// testMailbox serves the shipping mail fixture over plain IMAP.
func testMailbox(t *testing.T) mailSettings {
	t.Helper()
	raw, err := os.ReadFile("internal/mail/testdata/shipping_mail.eml")
	if err != nil {
		t.Fatal(err)
	}
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("erika", "secret")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := user.Append("INBOX", literal{bytes.NewReader(raw)}, &imap.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(user)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}},
		InsecureAuth: true,
		Logger:       nopLogger{},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	cfg := map[string]string{
		keyMailHost: "127.0.0.1", keyMailPort: port, keyMailSecurity: "none",
		keyMailUser: "erika", keyMailPassword: "secret",
	}
	s, ok, err := mailSettingsFrom(cfg, 5*time.Second)
	if !ok || err != nil {
		t.Fatal(ok, err)
	}
	return s
}

func TestUpdateShipmentsFindsCodeInMail(t *testing.T) {
	ms := testMailbox(t)
	env := &trackEnv{mail: &ms, warn: func(err error) { t.Error(err) }}
	loans := []bauduu.Loan{{ID: 8, SKU: "31119-1", Name: "Riesenrad 3in1 31119", Shipped: day("2026-10-14"), Status: "Ausgeliehen"}}
	u := updateShipments(context.Background(), env, loans, nil, nil, time.Now())
	if len(u.Shipments) != 1 || u.Shipments[0].Code != "00340434698381831612" || u.Shipments[0].Carrier != "DHL" {
		t.Fatalf("shipments = %+v", u.Shipments)
	}
	if len(u.Changes) != 1 || u.Changes[0].Title != "Trackingnummer für Riesenrad 3in1" ||
		!strings.Contains(u.Changes[0].Line, "DHL 00340434698381831612 https://www.dhl.de/de/privatkunden/pakete-empfangen/verfolgen.html?piececode=00340434698381831612") {
		t.Errorf("changes = %+v", u.Changes)
	}
	if len(u.MailSeen) != 1 {
		t.Errorf("seen = %v", u.MailSeen)
	}
	// Known code, no pending set: the mailbox is not searched again.
	env.mail.Password = "wrong"
	if u = updateShipments(context.Background(), env, loans, u.Shipments, u.MailSeen, time.Now()); len(u.Changes) != 0 {
		t.Errorf("second run = %+v", u.Changes)
	}
}

func TestUpdateShipmentsWarnsAndKeepsState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, "upstream request failed")
	}))
	defer srv.Close()
	client, _ := dhl.New(srv.URL, 5*time.Second)
	var warnings []error
	env := &trackEnv{dhl: client, warn: func(err error) { warnings = append(warnings, err) }}
	loans := []bauduu.Loan{{ID: 8, SKU: "31119-1", Name: "Riesenrad 3in1 31119", Status: "Ausgeliehen"}}
	old := &dhl.Shipment{Status: "unterwegs"}
	prev := []trackedShipment{{LoanID: 8, Carrier: "DHL", Code: "C1", Tracking: old}}
	u := updateShipments(context.Background(), env, loans, prev, nil, time.Now())
	if len(warnings) != 1 || !strings.Contains(warnings[0].Error(), "502") {
		t.Errorf("warnings = %v", warnings)
	}
	if len(u.Changes) != 0 || u.Shipments[0].Tracking != old {
		t.Errorf("state lost: %+v", u)
	}
}

func TestUpdateShipmentsKeepsHistoryWhenDetailsFail(t *testing.T) {
	var mu sync.Mutex
	detailsDown := true
	events := []string{"angekündigt", "im Paketzentrum", "zugestellt"}
	queries := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/int-verfolgen/data/config":
			io.WriteString(w, `{"verfolgenCsrfToken":"t"}`)
		case "/int-verfolgen/data/search":
			// The public summary: current status, no history.
			queries++
			io.WriteString(w, `{"sendungen":[{"id":"C1","hasCompleteDetails":false,"sendungsdetails":{"istZugestellt":true,"sendungsverlauf":{"status":"zugestellt"}}}]}`)
		case "/int-verfolgen/data/shipment":
			if detailsDown {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			var ev []string
			for i, e := range events {
				ev = append(ev, fmt.Sprintf(`{"datum":"2026-11-0%dT10:00:00.000+01:00","status":%q}`, i+1, e))
			}
			fmt.Fprintf(w, `{"sendungen":[{"id":"C1","hasCompleteDetails":true,"sendungsdetails":{"istZugestellt":true,"sendungsverlauf":{"status":"zugestellt","events":[%s]}}}]}`, strings.Join(ev, ","))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client, _ := dhl.New(srv.URL, 5*time.Second)
	var warnings []error
	env := &trackEnv{dhl: client, zip: "44579", warn: func(err error) { warnings = append(warnings, err) }}
	loans := []bauduu.Loan{{ID: 8, SKU: "31119-1", Name: "Riesenrad 3in1 31119", Status: "Ausgeliehen"}}
	old := &dhl.Shipment{Status: "angekündigt", Complete: true, Events: []dhl.Event{{Status: "angekündigt"}}}
	prev := []trackedShipment{{LoanID: 8, Carrier: "DHL", Code: "C1", Tracking: old}}
	ctx := context.Background()

	u := updateShipments(ctx, env, loans, prev, nil, time.Now())
	if len(warnings) != 1 {
		t.Errorf("warnings = %v", warnings)
	}
	if tr := u.Shipments[0].Tracking; tr == nil || len(tr.Events) != 1 {
		t.Fatalf("history lost: %+v", tr)
	}
	// Delivered by the summary, but the details are still missing.
	if u.Shipments[0].done() {
		t.Fatal("done without details")
	}
	mu.Lock()
	detailsDown = false
	mu.Unlock()
	u = updateShipments(ctx, env, loans, u.Shipments, nil, time.Now())
	if len(u.Changes) != 1 || !strings.Contains(u.Changes[0].Line, "im Paketzentrum") || !strings.Contains(u.Changes[0].Line, "zugestellt (") {
		t.Errorf("after recovery = %+v", u.Changes)
	}
	before := queries
	if u = updateShipments(ctx, env, loans, u.Shipments, nil, time.Now()); queries != before || len(u.Changes) != 0 {
		t.Errorf("queried after delivery with details: %d → %d, %+v", before, queries, u.Changes)
	}
}

func TestTrackingChangesStatusTime(t *testing.T) {
	s := &trackedShipment{Code: "C1", Set: "Riesenrad 3in1 (31119)"}
	at := time.Date(2026, 11, 9, 10, 0, 0, 0, time.Local)
	old := &dhl.Shipment{Status: "im Paketzentrum bearbeitet", StatusTime: at}
	cur := *old
	if got := trackingChanges(s, old, &cur); len(got) != 0 {
		t.Errorf("unchanged = %+v", got)
	}
	cur.StatusTime = at.Add(20 * time.Hour)
	if got := trackingChanges(s, old, &cur); len(got) != 1 || !strings.Contains(got[0].Line, cur.StatusTime.Format("02.01. 15:04")) {
		t.Errorf("same status, new time = %+v", got)
	}
}

func TestTrackingChangesWindowEnd(t *testing.T) {
	s := &trackedShipment{Code: "C1", Set: "Riesenrad 3in1 (31119)"}
	from := time.Date(2026, 11, 9, 10, 0, 0, 0, time.Local)
	old := &dhl.Shipment{Status: "unterwegs", DeliveryFrom: from, DeliveryTo: from.Add(2 * time.Hour)}
	cur := *old
	cur.DeliveryTo = from.Add(4 * time.Hour)
	got := trackingChanges(s, old, &cur)
	if len(got) != 1 || !strings.Contains(got[0].Line, "10:00–14:00") {
		t.Errorf("changes = %+v", got)
	}
}

func TestSummarizeMixed(t *testing.T) {
	title, _ := summarize([]loanChange{{Title: "a", Line: "a"}, {Title: "b", Line: "b", Shipment: true}})
	if title != "Bauduu: 2 Änderungen" {
		t.Errorf("mixed = %q", title)
	}
	title, _ = summarize([]loanChange{{Title: "a", Line: "a", Shipment: true}, {Title: "b", Line: "b", Shipment: true}})
	if title != "Bauduu: 2 Neuigkeiten zur Sendung" {
		t.Errorf("shipments = %q", title)
	}
}

func TestUpdateConfigKeepsOtherEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("# mine\nuser = \"erika\"\npassword = pw\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := updateConfig(path, "", []kv{{keyMailHost, "imap.example.com"}, {keyDHLZip, "44579"}}); err != nil {
		t.Fatal(err)
	}
	if err := writeConfig(path, credentials{User: "erika", Password: "new pw"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg["user"] != "erika" || cfg["password"] != "new pw" || cfg[keyMailHost] != "imap.example.com" || cfg[keyDHLZip] != "44579" {
		t.Errorf("config = %v", cfg)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), "# mine\nuser = ") {
		t.Errorf("order/comments lost:\n%s", b)
	}
	if err := updateConfig(path, "", []kv{{keyDHLZip, ""}}); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := readConfig(path); cfg[keyDHLZip] != "" || cfg[keyMailHost] == "" {
		t.Errorf("remove = %v", cfg)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode().Perm())
	}
}

func TestUpdateConfigReplacesDuplicateKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("user = erika\npassword = a\ndhl_zip = 1\npassword = b\nDHL_ZIP = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := updateConfig(path, "", []kv{{"password", "new"}, {keyDHLZip, ""}}); err != nil {
		t.Fatal(err)
	}
	cfg, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg["password"] != "new" || cfg[keyDHLZip] != "" || cfg["user"] != "erika" {
		t.Errorf("config = %v", cfg)
	}
	if b, _ := os.ReadFile(path); strings.Count(string(b), "password") != 1 {
		t.Errorf("duplicates left:\n%s", b)
	}
}

func TestResolveZip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	os.WriteFile(path, []byte("dhl_zip = 44579\n"), 0o600)
	t.Setenv("BAUDUU_DHL_ZIP", "")
	if z, err := resolveZip("", path); z != "44579" || err != nil {
		t.Errorf("config zip = %q, %v", z, err)
	}
	t.Setenv("BAUDUU_DHL_ZIP", "10115")
	if z, _ := resolveZip("", path); z != "10115" {
		t.Errorf("env zip = %q", z)
	}
	if z, _ := resolveZip("80331", path); z != "80331" {
		t.Errorf("flag zip = %q", z)
	}
	if _, err := resolveZip("4457", path); err == nil {
		t.Error("4 digits must fail")
	}
}

func TestMailSettingsFrom(t *testing.T) {
	if _, ok, err := mailSettingsFrom(map[string]string{}, time.Second); ok || err != nil {
		t.Errorf("unconfigured = %v, %v", ok, err)
	}
	t.Setenv("BAUDUU_MAIL_PASSWORD", "envpw")
	s, ok, err := mailSettingsFrom(map[string]string{keyMailHost: "pop.example.com", keyMailProtocol: "pop3", keyMailUser: "u", keyMailPassword: "pw"}, time.Second)
	if !ok || err != nil || s.Password != "envpw" || s.Port != 995 || s.From != bauduu.ShippingSender {
		t.Errorf("pop3 = %+v, %v, %v", s, ok, err)
	}
	if _, _, err := mailSettingsFrom(map[string]string{keyMailHost: "h", keyMailPort: "x", keyMailUser: "u"}, time.Second); err == nil {
		t.Error("bad port must fail")
	}
}

func TestShipmentTable(t *testing.T) {
	s := trackedShipment{Set: "Riesenrad 3in1 (31119)", Carrier: "DHL", Code: "C1", Tracking: &dhl.Shipment{
		Status: "In Zustellung", Progress: 4, MaxProgress: 5, Complete: true, URL: dhl.TrackingURL("C1"),
		DeliveryFrom: at("2026-11-07 10:00"), DeliveryTo: at("2026-11-07 12:00"),
		Events: []dhl.Event{{Time: at("2026-11-06 14:01"), Status: "angekündigt"}, {Time: at("2026-11-07 08:00"), Location: "Dortmund", Status: "In Zustellung"}},
	}}
	var buf bytes.Buffer
	shipmentTable(s).render(&buf, false, 80)
	out := buf.String()
	for _, want := range []string{"Riesenrad 3in1 (31119) · DHL C1", "■■■■□ In Zustellung", "Zustellung voraussichtlich 07.11. 10:00–12:00", "2026-11-07 08:00"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
	// Newest event first.
	if strings.Index(out, "2026-11-07 08:00") > strings.Index(out, "2026-11-06 14:01") {
		t.Errorf("events not newest first:\n%s", out)
	}
}

func TestSetupMailNonInteractive(t *testing.T) {
	ms := testMailbox(t)
	path := filepath.Join(t.TempDir(), "config")
	os.WriteFile(path, []byte("user = \"erika\"\npassword = \"pw\"\n"), 0o600)
	opts := &options{configPath: path, timeout: 5 * time.Second}
	f := &setupMailFlags{protocol: "imap", host: ms.Host, port: ms.Port, security: "none", user: "erika", days: 3650}
	set := map[string]bool{"protocol": true, "host": true, "port": true, "security": true, "mail-user": true}
	changed := func(name string) bool { return set[name] }

	t.Setenv("BAUDUU_MAIL_PASSWORD", "wrong")
	var out bytes.Buffer
	if err := runSetupMail(context.Background(), &out, opts, f, changed); err == nil {
		t.Fatal("wrong password must fail")
	}
	if cfg, _ := readConfig(path); cfg[keyMailHost] != "" {
		t.Error("failed test must not save")
	}
	t.Setenv("BAUDUU_MAIL_PASSWORD", "secret")
	if err := runSetupMail(context.Background(), &out, opts, f, changed); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "1 Mails von bauduu@brickcoast.de in den letzten 3650 Tagen, 1 mit Trackingnummer") {
		t.Errorf("output = %s", out.String())
	}
	cfg, _ := readConfig(path)
	if cfg["user"] != "erika" || cfg[keyMailHost] != ms.Host || cfg[keyMailPassword] != "secret" || cfg[keyMailMailbox] != "INBOX" || cfg[keyMailFrom] != bauduu.ShippingSender {
		t.Errorf("config = %v", cfg)
	}
}

func TestNewTrackEnvLenient(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	// Mail settings without an account and a broken postal code.
	if err := os.WriteFile(path, []byte("mail_host = imap.example.com\ndhl_zip = 123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BAUDUU_DHL_ZIP", "")
	t.Setenv("BAUDUU_MAIL_PASSWORD", "")
	opts := &options{configPath: path, timeout: time.Second}
	if _, err := newTrackEnv(opts, "", false, false, false, nil); err == nil {
		t.Error("strict: no error")
	}
	if _, err := newTrackEnv(opts, "", true, false, false, nil); err == nil || !strings.Contains(err.Error(), "postal code") {
		t.Errorf("strict zip = %v", err)
	}
	env, err := newTrackEnv(opts, "", false, false, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if env.mail != nil || env.zip != "" || env.dhl == nil {
		t.Errorf("lenient env = %+v", env)
	}
}
