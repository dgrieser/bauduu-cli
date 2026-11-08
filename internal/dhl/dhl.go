// Package dhl reads the public DHL shipment tracking (dhl.de "Sendungsverfolgung").
//
// The tracking page is a single page app that loads its data as JSON from
// /int-verfolgen/data: "config" hands out a CSRF token, "search" returns the
// public status of a piece code and a POST to "shipment" with the recipient's
// postal code (Postleitzahl) unlocks the full details, such as the delivery
// time window and every event of the history.
package dhl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the site root of the tracking endpoints.
const DefaultBaseURL = "https://www.dhl.de/"

// DefaultUserAgent looks like a browser; the site's bot protection rejects
// some non-browser clients outright.
const DefaultUserAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0"

const (
	pathConfig   = "int-verfolgen/data/config"
	pathSearch   = "int-verfolgen/data/search"
	pathShipment = "int-verfolgen/data/shipment"
	pathPage     = "de/privatkunden/pakete-empfangen/verfolgen.html"
)

// TrackingURL is the public page of a piece code, for people to open.
func TrackingURL(code string) string {
	return DefaultBaseURL + pathPage + "?piececode=" + url.QueryEscape(code)
}

// Event is one entry of the shipment history.
type Event struct {
	Time     time.Time `json:"time"`
	Location string    `json:"location,omitempty"`
	Status   string    `json:"status"`
}

// Shipment is the tracking state of one piece code.
type Shipment struct {
	Code    string `json:"code"`
	Product string `json:"product,omitempty"`
	// Status is the current status text, StatusTime when it was reached.
	Status      string    `json:"status"`
	StatusTime  time.Time `json:"status_time,omitzero"`
	Progress    int       `json:"progress"`
	MaxProgress int       `json:"max_progress"`
	Delivered   bool      `json:"delivered"`
	NextStep    string    `json:"next_step,omitempty"`
	// DeliveryFrom/To is the announced delivery time window, if any.
	DeliveryFrom time.Time `json:"delivery_from,omitzero"`
	DeliveryTo   time.Time `json:"delivery_to,omitzero"`
	// Events are oldest first.
	Events []Event `json:"events"`
	// Complete is false while the site only shows the public summary.
	Complete bool `json:"complete"`
	// ZipRequired means the details need the recipient's postal code.
	ZipRequired bool `json:"zip_required"`
	// NotFound carries the reason when DHL knows nothing (yet) about the code.
	NotFound string `json:"not_found,omitempty"`
	URL      string `json:"url"`
}

// Fingerprint changes whenever something worth reporting changes.
func (s *Shipment) Fingerprint() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%t|%s|%d|%s|%s", s.Status, s.StatusTime.UTC(), s.Delivered, s.NotFound, len(s.Events), s.DeliveryFrom.UTC(), s.DeliveryTo.UTC())
	return b.String()
}

// Error is a refusal by the tracking service.
type Error struct {
	Status  int
	Message string
	// BlockedFor is set when the rate limiter blocks further queries.
	BlockedFor time.Duration
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("DHL: HTTP %d", e.Status)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.BlockedFor > 0 {
		msg += fmt.Sprintf(" (gesperrt für %s)", e.BlockedFor.Round(time.Second))
	}
	return msg
}

// Client queries the tracking service. It is not safe for concurrent use.
type Client struct {
	base      *url.URL
	http      *http.Client
	UserAgent string
	csrf      string
}

// New returns a client for baseURL (DefaultBaseURL if empty).
func New(baseURL string, timeout time.Duration) (*Client, error) {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DHL base URL: %w", err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &Client{
		base: base,
		http: &http.Client{
			Jar:     jar,
			Timeout: timeout,
			// Errors are answered with a redirect to an HTML error page;
			// report the redirect instead of parsing that page.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		UserAgent: DefaultUserAgent,
	}, nil
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	u, err := c.base.Parse(path)
	if err != nil {
		return err
	}
	if query == nil {
		query = url.Values{}
	}
	query.Set("language", "de")
	u.RawQuery = query.Encode()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Language", "de-DE,de;q=0.9")
	req.Header.Set("Referer", c.base.String()+pathPage)
	if c.csrf != "" {
		req.Header.Set("verfolgen-CSRF-token", c.csrf)
		req.Header.Set("verfolgen-wg", "0")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("DHL: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("DHL: read %s: %w", u.Path, err)
	}
	if tok := resp.Header.Get("Verfolgen-CSRF-Token"); tok != "" {
		c.csrf = tok
	}
	if resp.StatusCode/100 == 3 {
		return &Error{Status: resp.StatusCode, Message: "Anfrage abgewiesen (Weiterleitung auf " + resp.Header.Get("Location") + ")"}
	}
	if resp.StatusCode != http.StatusOK {
		return responseError(resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("DHL: parse %s: %w (%s)", u.Path, err, snippet(data))
	}
	return nil
}

// responseError turns an error answer into an *Error. The service sends
// {"error": "...", "blockTime": <unix millis>} for rejected queries.
func responseError(status int, data []byte) error {
	e := &Error{Status: status}
	var v struct {
		Error     string    `json:"error"`
		BlockTime flexValue `json:"blockTime"`
	}
	if json.Unmarshal(data, &v) == nil {
		e.Message = v.Error
		if ms, err := strconv.ParseInt(string(v.BlockTime), 10, 64); err == nil && ms > 0 {
			if d := time.Until(time.UnixMilli(ms)); d > 0 {
				e.BlockedFor = d
			}
		}
	} else {
		e.Message = snippet(data)
	}
	return e
}

func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

func (c *Client) ensureToken(ctx context.Context) error {
	if c.csrf != "" {
		return nil
	}
	var cfg struct {
		Token string `json:"verfolgenCsrfToken"`
	}
	if err := c.do(ctx, http.MethodGet, pathConfig, url.Values{"domain": {"de"}}, nil, &cfg); err != nil {
		return err
	}
	if cfg.Token == "" {
		return errors.New("DHL: no CSRF token in config")
	}
	c.csrf = cfg.Token
	return nil
}

// Track returns the status of code. With a postal code (zip) it also asks for
// the full details when the public summary does not include them.
func (c *Client) Track(ctx context.Context, code, zip string) (*Shipment, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, errors.New("DHL: empty tracking code")
	}
	if err := c.ensureToken(ctx); err != nil {
		return nil, err
	}
	var res searchResult
	q := url.Values{"piececode": {code}, "noRedirect": {"true"}}
	if err := c.do(ctx, http.MethodGet, pathSearch, q, nil, &res); err != nil {
		return nil, err
	}
	s, err := res.shipment(code)
	if err != nil {
		return nil, err
	}
	if zip == "" || (s.Complete && !s.ZipRequired) || s.NotFound != "" {
		return s, nil
	}
	var detail searchResult
	payload := map[string]any{"piececode": code, "zip": zip, "international": false}
	if err := c.do(ctx, http.MethodPost, pathShipment, nil, payload, &detail); err != nil {
		return s, fmt.Errorf("DHL: Details mit PLZ %s: %w", zip, err)
	}
	d, err := detail.shipment(code)
	if err != nil {
		return s, fmt.Errorf("DHL: Details mit PLZ %s: %w", zip, err)
	}
	return d, nil
}

// searchResult is the JSON answer of search and shipment.
type searchResult struct {
	Shipments []rawShipment `json:"sendungen"`
}

func (r *searchResult) shipment(code string) (*Shipment, error) {
	if len(r.Shipments) == 0 {
		return nil, fmt.Errorf("DHL: no shipment in answer for %s", code)
	}
	raw := r.Shipments[0]
	for _, s := range r.Shipments {
		if strings.EqualFold(string(s.ID), code) || strings.EqualFold(string(s.Info.SearchedCode), code) {
			raw = s
			break
		}
	}
	return raw.convert(code), nil
}

type rawShipment struct {
	ID          flexValue `json:"id"`
	Complete    bool      `json:"hasCompleteDetails"`
	ZipRequired bool      `json:"plzBenoetigt"`
	NotFound    *struct {
		NoData       bool      `json:"keineDatenVerfuegbar"`
		Invalid      bool      `json:"sendungsnummerUngueltig"`
		TooOld       bool      `json:"sendungsdatenZuAlt"`
		NotSearch    bool      `json:"sendungsnummerNichtSuchbar"`
		NotDHLParcel bool      `json:"keineDhlPaketSendung"`
		Text         flexValue `json:"fehlertext"`
	} `json:"sendungNichtGefunden"`
	Info struct {
		SearchedCode flexValue `json:"gesuchteSendungsnummer"`
		Name         flexValue `json:"sendungsname"`
	} `json:"sendungsinfo"`
	Details struct {
		Delivered   bool      `json:"istZugestellt"`
		ProductName flexValue `json:"produktName"`
		Delivery    struct {
			From flexValue `json:"zustellzeitfensterVon"`
			To   flexValue `json:"zustellzeitfensterBis"`
		} `json:"zustellung"`
		History struct {
			Status      flexValue `json:"status"`
			ShortStatus flexValue `json:"kurzStatus"`
			StatusDate  flexValue `json:"datumAktuellerStatus"`
			Progress    flexValue `json:"fortschritt"`
			MaxProgress flexValue `json:"maximalFortschritt"`
			NextStep    flexValue `json:"naechsterSchritt"`
			Events      []struct {
				Date     flexValue `json:"datum"`
				Location flexValue `json:"ort"`
				Status   flexValue `json:"status"`
			} `json:"events"`
		} `json:"sendungsverlauf"`
	} `json:"sendungsdetails"`
}

func (r rawShipment) convert(code string) *Shipment {
	h := r.Details.History
	s := &Shipment{
		Code:         code,
		Product:      string(r.Details.ProductName),
		Status:       firstNonEmpty(string(h.Status), string(h.ShortStatus)),
		StatusTime:   parseTime(string(h.StatusDate)),
		Progress:     atoi(string(h.Progress)),
		MaxProgress:  atoi(string(h.MaxProgress)),
		Delivered:    r.Details.Delivered,
		NextStep:     string(h.NextStep),
		DeliveryFrom: parseTime(string(r.Details.Delivery.From)),
		DeliveryTo:   parseTime(string(r.Details.Delivery.To)),
		Complete:     r.Complete,
		ZipRequired:  r.ZipRequired,
		URL:          TrackingURL(code),
	}
	if s.MaxProgress == 0 && s.Progress > 0 {
		s.MaxProgress = 5
	}
	for _, e := range h.Events {
		s.Events = append(s.Events, Event{
			Time:     parseTime(string(e.Date)),
			Location: strings.TrimSpace(string(e.Location)),
			Status:   strings.TrimSpace(string(e.Status)),
		})
	}
	if nf := r.NotFound; nf != nil {
		switch {
		case string(nf.Text) != "":
			s.NotFound = string(nf.Text)
		case nf.Invalid:
			s.NotFound = "Sendungsnummer ungültig"
		case nf.TooOld:
			s.NotFound = "Sendungsdaten zu alt"
		case nf.NotDHLParcel:
			s.NotFound = "keine DHL-Paket-Sendung"
		case nf.NotSearch:
			s.NotFound = "Sendungsnummer nicht suchbar"
		case nf.NoData:
			s.NotFound = "noch keine Daten verfügbar"
		}
	}
	if s.Status == "" && len(s.Events) > 0 {
		s.Status = s.Events[len(s.Events)-1].Status
	}
	return s
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

var timeLayouts = []string{
	"2006-01-02T15:04:05.000-07:00",
	time.RFC3339Nano,
	"2006-01-02T15:04:05.000Z0700",
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	time.DateOnly,
}

// berlin is used for timestamps without zone; the service is German.
var berlin = func() *time.Location {
	if loc, err := time.LoadLocation("Europe/Berlin"); err == nil {
		return loc
	}
	return time.Local
}()

func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil && ms > 1e11 {
		return time.UnixMilli(ms)
	}
	for _, l := range timeLayouts {
		if t, err := time.ParseInLocation(l, s, berlin); err == nil {
			return t
		}
	}
	return time.Time{}
}

// flexValue accepts a JSON string, number, bool or null as text, so a type
// change in the undocumented API does not break parsing.
type flexValue string

func (f *flexValue) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case len(b) == 0 || string(b) == "null":
		*f = ""
	case b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexValue(s)
	case b[0] == '{' || b[0] == '[':
		*f = ""
	default:
		*f = flexValue(b)
	}
	return nil
}
