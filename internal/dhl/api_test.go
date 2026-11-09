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

// apiJSON follows the Unified Tracking API; events come newest first and
// parcel-de puts its status code into status.
const apiJSON = `{"shipments":[{"id":"00340434698381831612","service":"parcel-de",
 "destination":{"address":{"countryCode":"DE","postalCode":"32312","addressLocality":"Lübbecke"}},
 "status":{"timestamp":"2026-11-06T16:41:00","statusCode":"delivered","status":"ZU","description":"Die Sendung wurde zugestellt."},
 "details":{"product":{"productName":"DHL Paket"},
  "sender":{"organizationName":"Bauduu & BrickCoast Steinebande gGmbH"},
  "receiver":{"name":"Erika Mustermann"},
  "proofOfDelivery":{"signed":{"name":"Empfänger:in"}}},
 "events":[
  {"timestamp":"2026-11-06T16:41:00","statusCode":"delivered","status":"ZU","description":"Die Sendung wurde zugestellt."},
  {"timestamp":"2026-11-06T00:32:00","location":{"address":{"addressLocality":"Bielefeld"}},"statusCode":"transit","remark":"Die Sendung ist in der Region des Empfängers angekommen."},
  {"timestamp":"2026-11-05T16:31:00","location":{"address":{"addressLocality":"Bochum"}},"statusCode":"transit","description":"Die Sendung wurde von DHL bearbeitet."}]}]}`

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
	if s.Source != SourceAPI || !s.Delivered || s.Status != "Die Sendung wurde zugestellt." || s.StatusTime.Hour() != 16 {
		t.Fatalf("api = %+v", s)
	}
	if s.Product != "DHL Paket" || s.Sender != "Bauduu & BrickCoast Steinebande gGmbH" || s.Recipient != "Erika Mustermann" ||
		s.Destination != "32312 Lübbecke" || s.DeliveredTo != "Empfänger:in" {
		t.Errorf("details = %+v", s)
	}
	if len(s.Events) != 3 || s.Events[0].Location != "Bochum" || s.Events[1].Status != "Die Sendung ist in der Region des Empfängers angekommen." || s.Events[2].Status != "Die Sendung wurde zugestellt." {
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
