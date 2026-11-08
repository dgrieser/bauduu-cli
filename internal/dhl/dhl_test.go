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

// The public answer: summary only, details need the postal code.
const searchJSON = `{"sendungen":[{"id":"00340434698381831612","hasCompleteDetails":false,"plzBenoetigt":true,
 "sendungsinfo":{"gesuchteSendungsnummer":"00340434698381831612","sendungsrichtung":"ANKOMMEND"},
 "sendungsdetails":{"istZugestellt":false,"quelle":"SVB",
  "sendungsverlauf":{"status":"Die Sendung wurde im Start-Paketzentrum bearbeitet.","datumAktuellerStatus":"2026-11-06T18:22:00.000+01:00","fortschritt":2,"maximalFortschritt":5,"events":[]}}}]}`

const detailJSON = `{"sendungen":[{"id":"00340434698381831612","hasCompleteDetails":true,"plzBenoetigt":false,
 "sendungsinfo":{"gesuchteSendungsnummer":"00340434698381831612"},
 "sendungsdetails":{"istZugestellt":true,"produktName":"DHL Paket",
  "zustellung":{"zustellzeitfensterVon":"2026-11-07T10:00:00.000+01:00","zustellzeitfensterBis":"2026-11-07T12:00:00.000+01:00"},
  "sendungsverlauf":{"status":"Die Sendung wurde erfolgreich zugestellt.","datumAktuellerStatus":"2026-11-07T11:04:00.000+01:00","fortschritt":"5","maximalFortschritt":5,
   "events":[
    {"datum":"2026-11-06T14:01:00.000+01:00","ort":"","status":"Die Sendung wurde elektronisch angekündigt."},
    {"datum":"2026-11-06T18:22:00.000+01:00","ort":"Dortmund","status":"Die Sendung wurde im Start-Paketzentrum bearbeitet."},
    {"datum":"2026-11-07T11:04:00.000+01:00","ort":"44579 Castrop-Rauxel","status":"Die Sendung wurde erfolgreich zugestellt."}]}}}]}`

func fakeDHL(t *testing.T, wantZip string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /int-verfolgen/data/config", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"verfolgenCsrfToken":"tok-1","currentLanguage":"de"}`)
	})
	check := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("verfolgen-CSRF-token") != "tok-1" || r.URL.Query().Get("language") != "de" {
			http.Redirect(w, r, "/de/toolbar/errors/fehlermeldung.html", http.StatusSeeOther)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /int-verfolgen/data/search", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		if r.URL.Query().Get("piececode") != code {
			io.WriteString(w, `{"sendungen":[{"id":"x","sendungNichtGefunden":{"sendungsnummerUngueltig":true}}]}`)
			return
		}
		io.WriteString(w, searchJSON)
	})
	mux.HandleFunc("POST /int-verfolgen/data/shipment", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		var body struct {
			Piececode string `json:"piececode"`
			Zip       string `json:"zip"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Piececode != code {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		if body.Zip == "00000" {
			io.WriteString(w, `{"sendungen":[]}`)
			return
		}
		if body.Zip != wantZip {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"Ihre Eingabe war fehlerhaft."}`)
			return
		}
		io.WriteString(w, detailJSON)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestTrackPublic(t *testing.T) {
	srv := fakeDHL(t, "44579")
	c, err := New(srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Track(context.Background(), code, "")
	if err != nil {
		t.Fatal(err)
	}
	if !s.ZipRequired || s.Complete || s.Delivered || s.Progress != 2 || s.MaxProgress != 5 {
		t.Errorf("public = %+v", s)
	}
	if s.Status != "Die Sendung wurde im Start-Paketzentrum bearbeitet." || s.StatusTime.IsZero() {
		t.Errorf("status = %q at %v", s.Status, s.StatusTime)
	}
	if !strings.HasSuffix(s.URL, "verfolgen.html?piececode="+code) {
		t.Errorf("url = %q", s.URL)
	}
}

func TestTrackWithZip(t *testing.T) {
	srv := fakeDHL(t, "44579")
	c, _ := New(srv.URL, 5*time.Second)
	s, err := c.Track(context.Background(), code, "44579")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Complete || !s.Delivered || s.Progress != 5 || len(s.Events) != 3 || s.Product != "DHL Paket" {
		t.Fatalf("details = %+v", s)
	}
	if e := s.Events[2]; e.Location != "44579 Castrop-Rauxel" || e.Time.Hour() != 11 {
		t.Errorf("last event = %+v", e)
	}
	if s.DeliveryFrom.IsZero() || s.DeliveryTo.Sub(s.DeliveryFrom) != 2*time.Hour {
		t.Errorf("window = %v – %v", s.DeliveryFrom, s.DeliveryTo)
	}

	// A wrong postal code keeps the public summary and reports the refusal.
	s, err = c.Track(context.Background(), code, "12345")
	var de *Error
	if !errors.As(err, &de) || de.Status != http.StatusBadRequest || !strings.Contains(err.Error(), "fehlerhaft") {
		t.Errorf("wrong zip err = %v", err)
	}
	if s == nil || s.Complete {
		t.Errorf("wrong zip shipment = %+v", s)
	}

	// An answer without the shipment is an error too, with the summary.
	s, err = c.Track(context.Background(), code, "00000")
	if err == nil || !strings.Contains(err.Error(), "no shipment") || s == nil || s.Complete {
		t.Errorf("empty details = %+v, %v", s, err)
	}
}

func TestTrackNotFoundAndRefused(t *testing.T) {
	srv := fakeDHL(t, "44579")
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
