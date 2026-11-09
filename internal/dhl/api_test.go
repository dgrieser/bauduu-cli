package dhl

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// apiJSON is a real parcel-de answer with the name replaced: events come
// newest first, places carry the country, status holds DHL's own code
// (PO: in the delivery van) and the text is in description and remark.
const apiJSON = `{"shipments":[{"id":"00340434698381831612","service":"parcel-de",
 "origin":{"address":{"countryCode":"DE"}},"destination":{"address":{"countryCode":"DE"}},
 "status":{"timestamp":"2026-11-06T16:41:00","location":{"address":{"addressLocality":"Deutschland"}},"statusCode":"delivered","status":"ZU","statusDetailed":"DLVRD_ACCPT_ZU","description":"Die Sendung wurde zugestellt.","remark":"Die Sendung wurde zugestellt."},
 "details":{"product":{"productName":"DHL PAKET"},"receiver":{"@type":"Person","name":"Erika Mustermann"},"proofOfDeliverySignedAvailable":false,"totalNumberOfPieces":1,"weight":{"unitText":"kg","value":9}},
 "events":[
  {"timestamp":"2026-11-06T16:41:00","location":{"address":{"addressLocality":"Deutschland"}},"statusCode":"delivered","status":"ZU","description":"Die Sendung wurde zugestellt."},
  {"timestamp":"2026-11-06T09:57:00","statusCode":"transit","status":"PO","description":"Die Sendung wurde in das Zustellfahrzeug geladen. Die Zustellung erfolgt voraussichtlich heute."},
  {"timestamp":"2026-11-06T00:32:00","location":{"address":{"addressLocality":"Bielefeld, Deutschland"}},"statusCode":"transit","status":"EE","remark":"Die Sendung ist in der Region des Empfängers angekommen."},
  {"timestamp":"2026-11-05T18:23:00","statusCode":"pre-transit","status":"VA","description":"Die Sendung wurde elektronisch angekündigt."},
  {"timestamp":"2026-11-05T16:31:00","location":{"address":{"addressLocality":"Bochum, Deutschland"}},"statusCode":"transit","status":"AA","description":"Die Sendung wurde von DHL bearbeitet."}]}]}`

// fakeAPI answers like the Unified Tracking API; status selects the
// answer, calls records the request times.
type fakeAPI struct {
	mu     sync.Mutex
	status int
	calls  []time.Time
	query  []string
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, time.Now())
	f.query = append(f.query, r.URL.RawQuery)
	if r.URL.Path != "/track/shipments" {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("DHL-API-Key") != "good" {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"status":401,"title":"Unauthorized","detail":"Unauthorized for given resource."}`)
		return
	}
	w.Header().Set("Content-Type", "application/problem+json")
	switch {
	case f.status == http.StatusTooManyRequests:
		w.WriteHeader(f.status)
		io.WriteString(w, `{"status":429,"title":"Too many requests"}`)
	case f.status != 0 || r.URL.Query().Get("trackingNumber") != code:
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"status":404,"title":"No result found","detail":"No shipment with given tracking number found."}`)
	default:
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, apiJSON)
	}
}

func newAPIClient(t *testing.T, f *fakeAPI) *Client {
	t.Helper()
	api := httptest.NewServer(f)
	t.Cleanup(api.Close)
	c, err := New(fakeDHL(t).URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.APIURL, c.APIKey, c.APIInterval = api.URL, "good", 100*time.Millisecond
	return c
}

func TestTrackAPI(t *testing.T) {
	f := &fakeAPI{}
	c := newAPIClient(t, f)
	s, err := c.Track(context.Background(), code, "32312")
	if err != nil {
		t.Fatal(err)
	}
	if s.Source != SourceAPI || !s.Delivered || s.Phase != PhaseDelivered || s.Status != "Die Sendung wurde zugestellt." || s.StatusTime.Hour() != 16 {
		t.Fatalf("api = %+v", s)
	}
	if s.Product != "DHL PAKET" || s.Recipient != "Erika Mustermann" || s.Destination != "Deutschland" || s.Sender != "" {
		t.Errorf("details = %+v", s)
	}
	// Oldest first, home country dropped from the places.
	if len(s.Events) != 5 || s.Events[0].Location != "Bochum" || s.Events[2].Location != "Bielefeld" || s.Events[4].Location != "" ||
		s.Events[2].Status != "Die Sendung ist in der Region des Empfängers angekommen." || s.Events[4].Status != "Die Sendung wurde zugestellt." {
		t.Errorf("events = %+v", s.Events)
	}
	if q := f.query[0]; !strings.Contains(q, "recipientPostalCode=32312") || !strings.Contains(q, "service=parcel-de") {
		t.Errorf("query = %s", q)
	}
	// The second call waits for the rate limit.
	if _, err := c.Track(context.Background(), code, ""); err != nil {
		t.Fatal(err)
	}
	if d := f.calls[1].Sub(f.calls[0]); d < 90*time.Millisecond {
		t.Errorf("calls %v apart", d)
	}
}

func TestTrackAPIFallsBackToWeb(t *testing.T) {
	f := &fakeAPI{}
	c := newAPIClient(t, f)
	ctx := context.Background()

	// A bad key: data from dhl.de, with the reason.
	c.APIKey = "bad"
	s, err := c.Track(ctx, code, "")
	if !errors.Is(err, ErrAPIKey) || s == nil || s.Source != SourceWeb {
		t.Errorf("bad key = %+v, %v", s, err)
	}
	// Unknown to the API: dhl.de, no error.
	c.APIKey = "good"
	f.status = http.StatusNotFound
	if s, err = c.Track(ctx, code, ""); err != nil || s.Source != SourceWeb {
		t.Errorf("not found = %+v, %v", s, err)
	}
	// Rate limit: dhl.de, with the reason.
	f.status = http.StatusTooManyRequests
	if s, err = c.Track(ctx, code, ""); err == nil || !strings.Contains(err.Error(), "Abruflimit") || s.Source != SourceWeb {
		t.Errorf("rate limit = %+v, %v", s, err)
	}
}

func TestAPIPhase(t *testing.T) {
	for _, c := range []struct{ code, status, want string }{
		{"pre-transit", "VA", PhaseAnnounced},
		{"transit", "AA", PhaseTransit},
		{"transit", "PO", PhaseOutForDelivery},
		{"delivered", "ZU", PhaseDelivered},
		{"failure", "", PhaseFailure},
		{"unknown", "", PhaseUnknown},
	} {
		if got := (apiEvent{StatusCode: flexValue(c.code), Status: flexValue(c.status)}).phase(); got != c.want {
			t.Errorf("%s/%s = %q, want %q", c.code, c.status, got, c.want)
		}
	}
}

func TestAPIPlace(t *testing.T) {
	for _, c := range []struct{ locality, home, want string }{
		{"Bochum, Deutschland", "DE", "Bochum"},
		{"Deutschland", "DE", ""},
		{"Wien, Österreich", "DE", "Wien, Österreich"},
		{"Wien, Österreich", "AT", "Wien"},
		{"Bochum", "", "Bochum"},
	} {
		if got := domestic(c.locality, c.home); got != c.want {
			t.Errorf("domestic(%q, %q) = %q, want %q", c.locality, c.home, got, c.want)
		}
	}
	if got := (apiAddress{PostalCode: "32312", Locality: "Lübbecke", CountryCode: "DE"}).place(""); got != "32312 Lübbecke" {
		t.Errorf("address = %q", got)
	}
	if got := (apiAddress{CountryCode: "AT"}).place("DE"); got != "Österreich" {
		t.Errorf("foreign country = %q", got)
	}
	if got := (apiAddress{CountryCode: "DE"}).place("DE"); got != "" {
		t.Errorf("home country = %q", got)
	}
}

func TestCheckAPIKey(t *testing.T) {
	c := newAPIClient(t, &fakeAPI{})
	if err := c.CheckAPIKey(context.Background()); err != nil {
		t.Errorf("good key = %v", err)
	}
	c.APIKey = "bad"
	if err := c.CheckAPIKey(context.Background()); !errors.Is(err, ErrAPIKey) {
		t.Errorf("bad key = %v", err)
	}
}
