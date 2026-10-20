package bauduu

import (
	"encoding/json"
	"strings"
	"time"
)

// Date is a calendar day without time of day. The site renders "no date" as
// 0000-00-00 or 30/11/-0001; both parse to the zero Date.
type Date struct {
	time.Time
}

func (d Date) String() string {
	if d.IsZero() {
		return ""
	}
	return d.Format(time.DateOnly)
}

func (d Date) MarshalJSON() ([]byte, error) {
	if d.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(d.Format(time.DateOnly))
}

// DaysUntil returns the whole days from d to end, or -1 if d is unset.
func (d Date) DaysUntil(end time.Time) int {
	if d.IsZero() {
		return -1
	}
	y, m, day := end.Date()
	endDay := time.Date(y, m, day, 0, 0, 0, 0, time.UTC)
	return int(endDay.Sub(d.Time).Hours() / 24)
}

// WishlistItem is one entry of the Ausleihliste, the queue of sets the next
// shipment is picked from.
type WishlistItem struct {
	Position    int     `json:"position"`
	ProductID   int     `json:"product_id"`
	SetNumber   string  `json:"set_number"`
	SKU         string  `json:"sku,omitempty"`
	Name        string  `json:"name"`
	Theme       string  `json:"theme"`
	Package     string  `json:"package,omitempty"`
	Pieces      int     `json:"pieces,omitempty"`
	Minifigures *int    `json:"minifigures,omitempty"`
	Year        int     `json:"year,omitempty"`
	Rating      float64 `json:"rating,omitempty"`
	RatingCount int     `json:"rating_count,omitempty"`
	InStock     bool    `json:"in_stock"`
	URL         string  `json:"url"`
	ImageURL    string  `json:"image_url,omitempty"`
	Description string  `json:"description,omitempty"`
}

// Loan is one row of "Bisherige Sets" (meine-sets).
type Loan struct {
	ID          int    `json:"id"`
	SKU         string `json:"sku"`
	Name        string `json:"name"`
	Internal    string `json:"internal,omitempty"`
	Shipped     Date   `json:"shipped"`
	Returned    Date   `json:"returned"`
	Status      string `json:"status"`
	BonusBricks int    `json:"bonus_bricks"`
	// Placeholder marks the "W" rows ("Nächster Versand ab ..."), which record
	// a scheduled shipping date rather than a set.
	Placeholder bool `json:"placeholder"`
}

// Active reports whether the set is still at the customer.
func (l Loan) Active() bool {
	return !l.Placeholder && l.Returned.IsZero()
}

// Days returns how long the set was (or has been, if still active) out.
func (l Loan) Days(now time.Time) int {
	if l.Placeholder || l.Shipped.IsZero() {
		return -1
	}
	if l.Returned.IsZero() {
		return l.Shipped.DaysUntil(now)
	}
	return l.Shipped.DaysUntil(l.Returned.Time)
}

// SetNumber strips the "-1" revision suffix from the article number.
func (l Loan) SetNumber() string {
	n, _, _ := strings.Cut(l.SKU, "-")
	return n
}

// Subscription is one booking from "Deine Buchung" (PayPlans dashboard).
type Subscription struct {
	Title   string   `json:"title"`
	Key     string   `json:"key"`
	Created Date     `json:"created"`
	Status  string   `json:"status"`
	Expired bool     `json:"expired"`
	Prices  []string `json:"prices"`
	Start   Date     `json:"start"`
	End     Date     `json:"end"`
	Period  string   `json:"period,omitempty"`
}

// Field is one labelled value from "Deine Daten".
type Field struct {
	Section   string `json:"section"`
	Key       string `json:"key"`
	Label     string `json:"label"`
	Value     string `json:"value"`
	Sensitive bool   `json:"sensitive,omitempty"`
}

// Profile combines the account core data with the customer form fields.
type Profile struct {
	UserID         int     `json:"user_id,omitempty"`
	CustomerNumber string  `json:"customer_number,omitempty"`
	Name           string  `json:"name"`
	Username       string  `json:"username"`
	Email          string  `json:"email"`
	Registered     Date    `json:"registered"`
	LastVisit      Date    `json:"last_visit"`
	Fields         []Field `json:"fields"`
}

// Account is everything the CLI can read for one login.
type Account struct {
	Wishlist      []WishlistItem `json:"wishlist"`
	Loans         []Loan         `json:"loans"`
	Subscriptions []Subscription `json:"subscriptions"`
	Profile       *Profile       `json:"profile"`
}
