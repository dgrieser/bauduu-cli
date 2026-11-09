package dhl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// DefaultAPIURL is the base of DHL's official API (developer.dhl.com).
const DefaultAPIURL = "https://api-eu.dhl.com/"

// DefaultAPIInterval is the free plan's limit: one call every five seconds.
const DefaultAPIInterval = 5 * time.Second

// ErrAPIKey means the API refused the key.
var ErrAPIKey = errors.New("DHL-API: Schlüssel ungültig")

// errAPINotFound means the API does not know the tracking code.
var errAPINotFound = errors.New("DHL-API: Sendung nicht gefunden")

// CheckAPIKey reports whether the API accepts c.APIKey: nil when it does,
// an error wrapping ErrAPIKey when it refuses it. It costs one API call.
func (c *Client) CheckAPIKey(ctx context.Context) error {
	if c.APIKey == "" {
		return ErrAPIKey
	}
	// An unknown code is enough: a valid key gets "not found".
	var res apiResult
	err := c.apiGet(ctx, url.Values{"trackingNumber": {"00340000000000000000"}, "service": {"parcel-de"}}, &res)
	if err == nil || errors.Is(err, errAPINotFound) {
		return nil
	}
	return err
}

func (c *Client) trackAPI(ctx context.Context, code, zip string) (*Shipment, error) {
	q := url.Values{"trackingNumber": {code}, "service": {"parcel-de"}, "language": {"de"}}
	if zip != "" {
		q.Set("recipientPostalCode", zip)
	}
	var res apiResult
	if err := c.apiGet(ctx, q, &res); err != nil {
		return nil, err
	}
	if len(res.Shipments) == 0 {
		return nil, errAPINotFound
	}
	raw := res.Shipments[0]
	for _, s := range res.Shipments {
		if strings.EqualFold(string(s.ID), code) {
			raw = s
			break
		}
	}
	return raw.convert(code), nil
}

// apiGet calls the tracking endpoint, keeping to the rate limit.
func (c *Client) apiGet(ctx context.Context, query url.Values, out any) error {
	base := c.APIURL
	if base == "" {
		base = DefaultAPIURL
	}
	u, err := url.Parse(strings.TrimSuffix(base, "/") + "/track/shipments")
	if err != nil {
		return fmt.Errorf("parse DHL API URL: %w", err)
	}
	u.RawQuery = query.Encode()
	interval := c.APIInterval
	if interval <= 0 {
		interval = DefaultAPIInterval
	}
	if wait := interval - time.Since(c.apiLast); !c.apiLast.IsZero() && wait > 0 {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("DHL-API-Key", c.APIKey)
	req.Header.Set("Accept", "application/json")
	c.apiLast = time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("DHL-API: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("DHL-API: read: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("DHL-API: parse: %w (%s)", err, snippet(data))
		}
		return nil
	case http.StatusNotFound:
		return errAPINotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w (HTTP %d)", ErrAPIKey, resp.StatusCode)
	}
	// Errors are problem details (RFC 7807).
	var p struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	msg := snippet(data)
	if json.Unmarshal(data, &p) == nil && (p.Title != "" || p.Detail != "") {
		msg = strings.TrimSuffix(p.Title+": "+p.Detail, ": ")
		msg = strings.TrimPrefix(msg, ": ")
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		msg = "Abruflimit erreicht: " + msg
	}
	return fmt.Errorf("DHL-API: HTTP %d: %s", resp.StatusCode, msg)
}

// apiResult is the answer of the tracking endpoint. Only some fields are
// documented; every value is read leniently.
type apiResult struct {
	Shipments []apiShipment `json:"shipments"`
}

type apiAddress struct {
	CountryCode flexValue `json:"countryCode"`
	PostalCode  flexValue `json:"postalCode"`
	Locality    flexValue `json:"addressLocality"`
}

type apiPlace struct {
	Address apiAddress `json:"address"`
}

type apiEvent struct {
	Timestamp   flexValue `json:"timestamp"`
	Location    apiPlace  `json:"location"`
	StatusCode  flexValue `json:"statusCode"`
	Status      flexValue `json:"status"`
	Description flexValue `json:"description"`
	Remark      flexValue `json:"remark"`
	NextSteps   flexValue `json:"nextSteps"`
}

type apiParty struct {
	Name         flexValue `json:"name"`
	Organization flexValue `json:"organizationName"`
	GivenName    flexValue `json:"givenName"`
	FamilyName   flexValue `json:"familyName"`
}

type apiShipment struct {
	ID          flexValue `json:"id"`
	Destination apiPlace  `json:"destination"`
	Status      apiEvent  `json:"status"`
	ETA         flexValue `json:"estimatedTimeOfDelivery"`
	TimeFrame   struct {
		From    flexValue `json:"estimatedFrom"`
		Through flexValue `json:"estimatedThrough"`
	} `json:"estimatedDeliveryTimeFrame"`
	Details struct {
		Product struct {
			Name flexValue `json:"productName"`
		} `json:"product"`
		Sender          apiParty `json:"sender"`
		Receiver        apiParty `json:"receiver"`
		ProofOfDelivery struct {
			Signed apiParty `json:"signed"`
		} `json:"proofOfDelivery"`
	} `json:"details"`
	Events []apiEvent `json:"events"`
}

// text is the event's description. parcel-de puts a status code ("ZU")
// into status and the text into description or remark.
func (e apiEvent) text() string {
	return clean(firstNonEmpty(string(e.Description), string(e.Remark), string(e.Status)))
}

// place is "32312 Lübbecke", or what there is of it.
func (a apiAddress) place() string {
	p := clean(string(a.PostalCode) + " " + string(a.Locality))
	if p == "" {
		p = clean(string(a.CountryCode))
	}
	return p
}

func (p apiParty) name() string {
	if n := clean(string(p.Name)); n != "" {
		return n
	}
	if n := clean(string(p.GivenName) + " " + string(p.FamilyName)); n != "" {
		return n
	}
	return clean(string(p.Organization))
}

func (r apiShipment) convert(code string) *Shipment {
	s := &Shipment{
		Code:        code,
		Source:      SourceAPI,
		Product:     clean(string(r.Details.Product.Name)),
		Sender:      r.Details.Sender.name(),
		Recipient:   r.Details.Receiver.name(),
		Destination: r.Destination.Address.place(),
		DeliveredTo: r.Details.ProofOfDelivery.Signed.name(),
		Status:      r.Status.text(),
		StatusTime:  parseTime(string(r.Status.Timestamp)),
		Delivered:   strings.EqualFold(string(r.Status.StatusCode), "delivered"),
		NextStep:    clean(string(r.Status.NextSteps)),
		URL:         TrackingURL(code),
	}
	s.DeliveryFrom = parseTime(string(r.TimeFrame.From))
	s.DeliveryTo = parseTime(string(r.TimeFrame.Through))
	if s.DeliveryFrom.IsZero() {
		s.DeliveryFrom = parseTime(string(r.ETA))
	}
	for _, e := range r.Events {
		s.Events = append(s.Events, Event{
			Time:     parseTime(string(e.Timestamp)),
			Location: clean(string(e.Location.Address.Locality)),
			Status:   e.text(),
		})
	}
	sort.SliceStable(s.Events, func(i, j int) bool { return s.Events[i].Time.Before(s.Events[j].Time) })
	if n := len(s.Events); n > 0 && s.Status == "" {
		s.Status, s.StatusTime = s.Events[n-1].Status, s.Events[n-1].Time
	}
	return s
}

// clean collapses runs of white space, which DHL's texts sometimes contain.
func clean(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
