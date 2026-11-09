package dhl

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const code = "00340434698381831612"

// The public answer, as dhl.de sends it for a delivered parcel.
const searchJSON = `{"sendungen":[{"id":"00340434698381831612","hasCompleteDetails":true,
 "sendungsinfo":{"gesuchteSendungsnummer":"00340434698381831612","sendungsrichtung":"ANKOMMEND"},
 "sendungsdetails":{"istZugestellt":true,"produktName":"DHL Paket","zielland":"Deutschland","quelle":"PAKET",
  "versender":{"name":"Bauduu & BrickCoast Steinebande gGmbH"},
  "zustellung":{"empfaenger":{"name":"Empfänger:in"},"zugestelltAnEmpfaenger":true},
  "sendungsverlauf":{"status":"Zustellung erfolgreich.","datumAktuellerStatus":"2026-11-06T16:41:10+01:00","fortschritt":5,"maximalFortschritt":5,
   "events":[
    {"datum":"2026-11-05T16:31:54+01:00","status":"Die Sendung wurde von DHL bearbeitet.","ruecksendung":false},
    {"datum":"2026-11-06T09:57:05+01:00","status":"Die Sendung wurde in das Zustellfahrzeug geladen. Die Zustellung  erfolgt voraussichtlich heute.","ruecksendung":false},
    {"datum":"2026-11-06T16:41:10+01:00","status":"Die Sendung wurde zugestellt.","ruecksendung":false}]}}}]}`

func fakeDHL(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /int-verfolgen/data/config", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"verfolgenCsrfToken":"tok-1","currentLanguage":"de"}`)
	})
	mux.HandleFunc("GET /int-verfolgen/data/search", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("verfolgen-CSRF-token") != "tok-1" || r.URL.Query().Get("language") != "de" {
			http.Redirect(w, r, "/de/toolbar/errors/fehlermeldung.html", http.StatusSeeOther)
			return
		}
		if r.URL.Query().Get("piececode") != code {
			io.WriteString(w, `{"sendungen":[{"id":"x","sendungNichtGefunden":{"sendungsnummerUngueltig":true}}]}`)
			return
		}
		io.WriteString(w, searchJSON)
	})
	// The details are for browsers only.
	mux.HandleFunc("POST /int-verfolgen/data/shipment", func(w http.ResponseWriter, r *http.Request) {
		t.Error("details requested from dhl.de")
		w.WriteHeader(http.StatusNotAcceptable)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestTrackWeb(t *testing.T) {
	srv := fakeDHL(t)
	c, err := New(srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Track(context.Background(), code, "44579")
	if err != nil {
		t.Fatal(err)
	}
	if s.Source != SourceWeb || !s.Delivered || s.Phase != PhaseDelivered || s.Progress != 5 || s.MaxProgress != 5 || len(s.Events) != 3 {
		t.Fatalf("web = %+v", s)
	}
	if s.Status != "Zustellung erfolgreich." || s.StatusTime.IsZero() {
		t.Errorf("status = %q at %v", s.Status, s.StatusTime)
	}
	if s.Product != "DHL Paket" || s.Sender != "Bauduu & BrickCoast Steinebande gGmbH" || s.Destination != "Deutschland" || s.DeliveredTo != "Empfänger:in" {
		t.Errorf("details = %+v", s)
	}
	if e := s.Events[1]; e.Status != "Die Sendung wurde in das Zustellfahrzeug geladen. Die Zustellung erfolgt voraussichtlich heute." || e.Location != "" {
		t.Errorf("event = %+v", e)
	}
	if !strings.HasSuffix(s.URL, "verfolgen.html?piececode="+code) {
		t.Errorf("url = %q", s.URL)
	}
}

func TestWebPhase(t *testing.T) {
	for _, c := range []struct {
		progress, max int
		delivered     bool
		want          string
	}{
		{0, 0, false, PhaseUnknown},
		{1, 5, false, PhaseAnnounced},
		{2, 5, false, PhaseTransit},
		{3, 5, false, PhaseTransit},
		{4, 5, false, PhaseOutForDelivery},
		{5, 5, true, PhaseDelivered},
		{4, 7, false, PhaseTransit},
	} {
		if got := webPhase(&Shipment{Progress: c.progress, MaxProgress: c.max, Delivered: c.delivered}); got != c.want {
			t.Errorf("%d/%d = %q, want %q", c.progress, c.max, got, c.want)
		}
	}
}

func TestTrackNotFoundAndRefused(t *testing.T) {
	srv := fakeDHL(t)
	c, _ := New(srv.URL, 5*time.Second)
	s, err := c.Track(context.Background(), "123", "44579")
	if err != nil || s.NotFound != "Sendungsnummer ungültig" {
		t.Errorf("not found = %+v, %v", s, err)
	}
	// A lost token is answered with a redirect, reported as such.
	c.csrf = "stale"
	_, err = c.Track(context.Background(), code, "")
	var de *Error
	if !errors.As(err, &de) || de.Status != http.StatusSeeOther || !strings.Contains(err.Error(), "fehlermeldung") {
		t.Errorf("redirect err = %v", err)
	}
}

func TestResponseErrorBlockTime(t *testing.T) {
	until := time.Now().Add(5 * time.Minute).UnixMilli()
	err := responseError(429, []byte(`{"error":"Zu viele Anfragen","blockTime":`+jsonInt(until)+`}`))
	var de *Error
	if !errors.As(err, &de) || de.BlockedFor < 4*time.Minute || de.Message != "Zu viele Anfragen" {
		t.Errorf("err = %#v", err)
	}
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestFingerprint(t *testing.T) {
	a := &Shipment{Status: "x", Events: []Event{{Status: "x"}}}
	b := *a
	if a.Fingerprint() != b.Fingerprint() {
		t.Error("same state, different fingerprint")
	}
	b.Events = append(b.Events, Event{Status: "y"})
	if a.Fingerprint() == b.Fingerprint() {
		t.Error("new event must change the fingerprint")
	}
}
