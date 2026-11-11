package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dgrieser/bauduu-cli/internal/bauduu"
	"github.com/dgrieser/bauduu-cli/internal/dhl"
)

func defaultReturns() returnSettings {
	r, _ := returnSettingsFrom(map[string]string{})
	return r
}

func TestReturnSettings(t *testing.T) {
	r := defaultReturns()
	if len(r.From) != 1 || r.From[0] != "packstation@dhl.de" || r.Recipient != "Bauduu" || r.Zip != "44579" {
		t.Errorf("defaults = %+v", r)
	}
	r, err := returnSettingsFrom(map[string]string{keyReturnFrom: "packstation@dhl.de, noreply@dhl.de;x@y", keyReturnName: "BrickCoast", keyReturnZip: "12345"})
	if err != nil || len(r.From) != 3 || r.From[1] != "noreply@dhl.de" || r.Recipient != "BrickCoast" || r.Zip != "12345" {
		t.Errorf("custom = %+v, %v", r, err)
	}
	if _, err := returnSettingsFrom(map[string]string{keyReturnZip: "123"}); err == nil {
		t.Error("bad zip must fail")
	}
}

func TestCheckRecipient(t *testing.T) {
	r := defaultReturns()
	cases := []struct {
		name string
		text string
		tr   *dhl.Shipment
		want int
	}{
		{"nothing known", "Packstation 119, 32312 Lübbecke", nil, recipientUnknown},
		{"api without receiver", "", &dhl.Shipment{Destination: "Deutschland"}, recipientUnknown},
		{"mail names address", "Empfänger: Bauduu Steinebande, 44579 Castrop-Rauxel", nil, recipientConfirmed},
		{"mail names address unlabeled", "An Bauduu Steinebande, 44579 Castrop-Rauxel", nil, recipientConfirmed},
		{"mail names only name", "Bauduu", nil, recipientUnknown},
		{"api names Bauduu", "", &dhl.Shipment{Recipient: "BAUDUU & BrickCoast gGmbH"}, recipientConfirmed},
		{"api names someone else", "", &dhl.Shipment{Recipient: "Amazon Retouren"}, recipientOther},
		{"other postal code", "", &dhl.Shipment{Destination: "10115 Berlin"}, recipientOther},
		{"right postal code", "", &dhl.Shipment{Destination: "44579 Castrop-Rauxel"}, recipientUnknown},
		{"receipt names other recipient", "Empfänger: Amazon Retouren\n04435 Schkeuditz\nAbgabestelle\nPackstation 119, 32312 Lübbecke", nil, recipientOther},
		{"receipt names other postal code", "Empfänger: Bauduu\n10115 Berlin", nil, recipientOther},
		{"receipt names multiline other", "Empfänger:\nBauduu\nMusterstraße 1\n10115 Berlin", nil, recipientOther},
		{"receipt Bauduu, DHL other", "Empfänger: Bauduu\n44579 Castrop-Rauxel", &dhl.Shipment{Recipient: "Amazon Retouren"}, recipientOther},
		{"receipt Bauduu", "Empfänger:\nBauduu Steinebande\n44579 Castrop-Rauxel\nPackstation 119, 32312 Lübbecke", nil, recipientConfirmed},
	}
	for _, c := range cases {
		if got := checkRecipient(r, c.text, c.tr); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

func TestReturnLoan(t *testing.T) {
	loans := []bauduu.Loan{
		{ID: 9, Name: "Neu 1", Shipped: day("2026-11-11"), Status: "Ausgeliehen"},
		{ID: 8, Name: "Riesenrad 3in1 31119", Shipped: day("2026-10-14"), Status: "Ausgeliehen"},
		{ID: 7, Name: "Alt", Shipped: day("2026-09-01"), Returned: day("2026-10-20"), Status: "Zurückgegeben"},
		{ID: 6, Name: "Älter", Shipped: day("2026-07-01"), Returned: day("2026-08-20"), Status: "Zurückgegeben"},
		{ID: 10, Name: "Nächster Versand", Placeholder: true},
	}
	cases := []struct {
		sent string
		want int
	}{
		// A set shipped the same day is not yet at home.
		{"2026-11-11 16:27", 8},
		{"2026-11-12 09:00", 9},
		// Before Riesenrad arrived the last set was "Alt", still at home.
		{"2026-10-13 10:00", 7},
		{"2026-06-01 10:00", 0},
	}
	for _, c := range cases {
		l, ok := returnLoan(loans, at(c.sent))
		if got := l.ID; !ok && c.want != 0 || ok && got != c.want {
			t.Errorf("%s: got %d (%v), want %d", c.sent, got, ok, c.want)
		}
	}
}

func TestUpdateReturnsFindsReceipt(t *testing.T) {
	ms := testMailboxWith(t, "shipping_mail.eml", "return_receipt.eml")
	env := &trackEnv{mail: &ms, returns: ms.Return, warn: func(err error) { t.Error(err) }}
	loans := []bauduu.Loan{{ID: 8, SKU: "31119-1", Name: "Riesenrad 3in1 31119", Shipped: day("2026-10-14"), Status: "Ausgeliehen"}}
	// The inbound parcel's code is never taken for a return.
	inbound := []trackedShipment{{LoanID: 8, Code: "00340434698381831612"}}
	now := at("2026-11-11 17:00")
	u := updateReturns(context.Background(), env, loans, nil, nil, inbound, now)
	if len(u.Returns) != 1 {
		t.Fatalf("returns = %+v", u.Returns)
	}
	r := u.Returns[0]
	if r.Code != "00340434358243873456" || r.LoanID != 8 || !r.Return || r.Confirmed || r.Set != "Riesenrad 3in1 (31119)" {
		t.Errorf("return = %+v", r)
	}
	if len(u.Changes) != 1 || u.Changes[0].Title != "Rücksendung Riesenrad 3in1 eingeliefert" || u.Changes[0].Set != "Rücksendung Riesenrad 3in1" ||
		!strings.Contains(u.Changes[0].Line, "00340434358243873456") || !strings.Contains(u.Changes[0].Line, "nicht bestätigt") {
		t.Errorf("changes = %+v", u.Changes)
	}
	if len(u.MailSeen) != 1 {
		t.Errorf("seen = %v", u.MailSeen)
	}
	// Known return: the mailbox is not searched again.
	env.mail.Password = "wrong"
	if u = updateReturns(context.Background(), env, loans, u.Returns, u.MailSeen, inbound, now); len(u.Changes) != 0 || len(u.Returns) != 1 {
		t.Errorf("second run = %+v", u)
	}
	// Once Bauduu booked the return and DHL delivered, the return is dropped.
	u.Returns[0].Tracking = &dhl.Shipment{Delivered: true}
	loans[0].Returned, loans[0].Status = day("2026-11-13"), "Zurückgegeben"
	if u = updateReturns(context.Background(), env, loans, u.Returns, u.MailSeen, nil, now); len(u.Returns) != 0 {
		t.Errorf("after return = %+v", u.Returns)
	}
}

func TestUpdateReturnsSkipsOtherRecipient(t *testing.T) {
	var zips []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zips = append(zips, r.URL.Query().Get("recipientPostalCode"))
		io.WriteString(w, `{"shipments":[{"id":"00340434358243873456","destination":{"address":{"countryCode":"DE"}},"status":{"timestamp":"2026-11-11T16:27:00","statusCode":"transit","description":"Eingeliefert."},
			"details":{"receiver":{"name":"Amazon Retouren"}},"events":[{"timestamp":"2026-11-11T16:27:00","description":"Eingeliefert."}]}]}`)
	}))
	defer api.Close()
	client, _ := dhl.New(api.URL, 5*time.Second)
	client.APIURL, client.APIKey, client.APIInterval = api.URL, "key", time.Millisecond
	ms := testMailboxWith(t, "return_receipt.eml")
	env := &trackEnv{mail: &ms, returns: ms.Return, dhl: client, warn: func(err error) { t.Error(err) }}
	loans := []bauduu.Loan{{ID: 8, Name: "Riesenrad 3in1 31119", Shipped: day("2026-10-14"), Status: "Ausgeliehen"}}
	u := updateReturns(context.Background(), env, loans, nil, nil, nil, at("2026-11-11 17:00"))
	if len(u.Returns) != 0 || len(u.Changes) != 0 || len(u.MailSeen) != 1 {
		t.Errorf("other recipient = %+v", u)
	}
	if len(zips) != 1 || zips[0] != "44579" {
		t.Errorf("queried with %v", zips)
	}
}

func TestReturnTrackingTitles(t *testing.T) {
	s := &trackedShipment{Set: "Riesenrad 3in1 (31119)", Code: "C1", Return: true}
	cur := &dhl.Shipment{Source: dhl.SourceAPI, Phase: dhl.PhaseTransit, Status: "unterwegs",
		Events: []dhl.Event{{Time: at("2026-11-12 08:00"), Location: "Bochum", Status: "bearbeitet"}}}
	ch := trackingChanges(s, nil, cur)
	if len(ch) != 1 || ch[0].Title != "Rücksendung Riesenrad 3in1 unterwegs (Bochum)" || !strings.HasPrefix(ch[0].Line, "DHL C1 (Rücksendung Riesenrad 3in1):") {
		t.Errorf("transit = %+v", ch)
	}
	done := &dhl.Shipment{Source: dhl.SourceAPI, Delivered: true, Status: "zugestellt", StatusTime: at("2026-11-13 10:00")}
	if ch := trackingChanges(s, cur, done); len(ch) != 1 || ch[0].Title != "Zugestellt: Rücksendung Riesenrad 3in1" {
		t.Errorf("delivered = %+v", ch)
	}
	// Receipt and first DHL status in one run share a title.
	transit := trackingChanges(s, nil, cur)
	title, _ := summarize([]loanChange{
		{Title: "Rücksendung Riesenrad 3in1 eingeliefert", Line: "a", Shipment: true, Set: "Rücksendung Riesenrad 3in1", Note: "Sendungsnummer da"},
		transit[0],
	})
	if title != "Bauduu: Rücksendung Riesenrad 3in1 unterwegs (Bochum), Sendungsnummer da" {
		t.Errorf("title = %q", title)
	}
}

func TestUpdateReturnsDropsDisprovedReturn(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"shipments":[{"id":"C9","status":{"timestamp":"2026-11-12T08:00:00","statusCode":"transit","description":"unterwegs"},
			"details":{"receiver":{"name":"Amazon Retouren"}},"events":[{"timestamp":"2026-11-12T08:00:00","description":"unterwegs"}]}]}`)
	}))
	defer api.Close()
	client, _ := dhl.New(api.URL, 5*time.Second)
	client.APIURL, client.APIKey, client.APIInterval = api.URL, "key", time.Millisecond
	env := &trackEnv{returns: defaultReturns(), dhl: client, warn: func(err error) { t.Error(err) }}
	loans := []bauduu.Loan{{ID: 8, SKU: "31119-1", Name: "Riesenrad 3in1 31119", Shipped: day("2026-10-14"), Status: "Ausgeliehen"}}
	prev := []trackedShipment{{LoanID: 8, Carrier: "DHL", Code: "C9", Return: true, MailDate: at("2026-11-11 16:27")}}
	u := updateReturns(context.Background(), env, loans, prev, nil, nil, at("2026-11-12 09:00"))
	if len(u.Returns) != 0 || len(u.Changes) != 1 || u.Changes[0].Title != "Rücksendung Riesenrad 3in1: falsche Sendung" {
		t.Errorf("disproved = %+v", u)
	}
}

func TestUpdateReturnsKeepsStateWhenOff(t *testing.T) {
	env := &trackEnv{returnsOff: true, warn: func(err error) { t.Error(err) }}
	prev := []trackedShipment{{LoanID: 8, Code: "C9", Return: true}}
	u := updateReturns(context.Background(), env, nil, prev, []string{"m1"}, nil, time.Now())
	if len(u.Returns) != 1 || len(u.MailSeen) != 1 || len(u.Changes) != 0 {
		t.Errorf("off = %+v", u)
	}
}
