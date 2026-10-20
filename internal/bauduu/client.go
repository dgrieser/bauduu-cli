package bauduu

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// DefaultBaseURL is the Joomla site root all paths below are relative to.
const DefaultBaseURL = "https://www.bauduu.de/lego/"

const (
	pathLogin     = "login"
	pathProfile   = "login/profile"
	pathWishlist  = "ausleihliste"
	pathLoans     = "meine-sets?limit=0&limitstart=0"
	pathDashboard = "my-dashboard"
	pathUserData  = "myuser"
	pathReturn    = "retour"
)

// ErrNotLoggedIn means the site served a page without an authenticated
// session, e.g. after the session expired.
var ErrNotLoggedIn = errors.New("not logged in")

// LoginError carries the site's own message for a rejected login.
type LoginError struct {
	Message string
}

func (e *LoginError) Error() string {
	if e.Message == "" {
		return "login failed"
	}
	return "login failed: " + e.Message
}

// Client talks to the bauduu.de customer area. It is safe for concurrent use
// once Login has returned.
type Client struct {
	base      *url.URL
	http      *http.Client
	userAgent string
	// detailWorkers bounds concurrent product page requests.
	detailWorkers int
	// Warn receives non-fatal problems, such as a product page that failed
	// to load; nil discards them.
	Warn func(error)
}

// New returns a client for baseURL (DefaultBaseURL if empty).
func New(baseURL string, timeout time.Duration, userAgent string) (*Client, error) {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base URL: %w", err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &Client{
		base:          base,
		http:          &http.Client{Jar: jar, Timeout: timeout},
		userAgent:     userAgent,
		detailWorkers: 4,
	}, nil
}

func (c *Client) do(req *http.Request) (*goquery.Document, error) {
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept-Language", "de-DE,de;q=0.9")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s %s: HTTP %d", req.Method, req.URL.Path, resp.StatusCode)
	}
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", req.URL.Path, err)
	}
	return doc, nil
}

func (c *Client) get(ctx context.Context, path string) (*goquery.Document, error) {
	u, err := c.base.Parse(path)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	return c.do(req)
}

// getAuthed fetches a customer page and fails if the session is gone, since
// the site answers those with a 200 marketing page instead of an error.
func (c *Client) getAuthed(ctx context.Context, path string) (*goquery.Document, error) {
	doc, err := c.get(ctx, path)
	if err != nil {
		return nil, err
	}
	if !isLoggedIn(doc) {
		return nil, fmt.Errorf("%s: %w", path, ErrNotLoggedIn)
	}
	return doc, nil
}

// Login authenticates via the Joomla com_users login form.
func (c *Client) Login(ctx context.Context, username, password string) error {
	doc, err := c.get(ctx, pathLogin)
	if err != nil {
		return fmt.Errorf("load login page: %w", err)
	}
	if isLoggedIn(doc) {
		return nil
	}
	token, err := loginToken(doc)
	if err != nil {
		return err
	}
	form := url.Values{
		"username": {username},
		"password": {password},
		"option":   {"com_users"},
		"task":     {"user.login"},
		token:      {"1"},
	}
	u, err := c.base.Parse(pathLogin)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	doc, err = c.do(req)
	if err != nil {
		return fmt.Errorf("submit login: %w", err)
	}
	if !isLoggedIn(doc) {
		return &LoginError{Message: systemMessage(doc)}
	}
	return nil
}

// Wishlist returns the Ausleihliste in queue order. With details it also
// loads each product page for article number, theme and rental package.
func (c *Client) Wishlist(ctx context.Context, details bool) ([]WishlistItem, error) {
	doc, err := c.getAuthed(ctx, pathWishlist)
	if err != nil {
		return nil, err
	}
	items, err := ParseWishlist(doc, c.base)
	if err != nil || !details {
		return items, err
	}
	sem := make(chan struct{}, c.detailWorkers)
	errs := make([]error, len(items))
	var wg sync.WaitGroup
	for i := range items {
		if items[i].URL == "" {
			continue
		}
		wg.Add(1)
		go func(item *WishlistItem, errp *error) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// Product pages are public, but the header still carries the
			// logout form for a live session; a missing one means the session
			// expired mid-run and the page cannot be trusted.
			pdoc, err := c.getAuthed(ctx, item.URL)
			if err != nil {
				*errp = fmt.Errorf("product details for %q: %w", item.Name, err)
				return
			}
			ParseProduct(pdoc).Apply(item)
		}(&items[i], &errs[i])
	}
	wg.Wait()
	// Details only enrich the list, so a failed product page is a warning
	// unless the context itself was cancelled.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, err := range errs {
		if err != nil && c.Warn != nil {
			c.Warn(err)
		}
	}
	return items, nil
}

// Loans returns the full "Bisherige Sets" history, newest first.
func (c *Client) Loans(ctx context.Context) ([]Loan, error) {
	doc, err := c.getAuthed(ctx, pathLoans)
	if err != nil {
		return nil, err
	}
	return ParseLoans(doc)
}

// Subscriptions returns the bookings from "Deine Buchung".
func (c *Client) Subscriptions(ctx context.Context) ([]Subscription, error) {
	doc, err := c.getAuthed(ctx, pathDashboard)
	if err != nil {
		return nil, err
	}
	return ParseSubscriptions(doc)
}

// Profile returns "Deine Daten" plus account metadata from the Joomla
// profile and the customer number from the Retoure form.
func (c *Client) Profile(ctx context.Context) (*Profile, error) {
	paths := []string{pathUserData, pathProfile, pathReturn}
	docs := make([]*goquery.Document, len(paths))
	errs := make([]error, len(paths))
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			docs[i], errs[i] = c.getAuthed(ctx, p)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	p := &Profile{}
	if err := parseProfileForm(docs[0], p); err != nil {
		return nil, err
	}
	parseProfileCore(docs[1], p)
	p.CustomerNumber = parseCustomerNumber(docs[2])
	return p, nil
}

// CustomerNumber returns the Kundennummer shown on the Retoure form.
func (c *Client) CustomerNumber(ctx context.Context) (string, error) {
	doc, err := c.getAuthed(ctx, pathReturn)
	if err != nil {
		return "", err
	}
	return parseCustomerNumber(doc), nil
}

// Account loads every section concurrently.
func (c *Client) Account(ctx context.Context, details bool) (*Account, error) {
	var (
		a    Account
		wg   sync.WaitGroup
		errs [4]error
	)
	wg.Add(4)
	go func() { defer wg.Done(); a.Wishlist, errs[0] = c.Wishlist(ctx, details) }()
	go func() { defer wg.Done(); a.Loans, errs[1] = c.Loans(ctx) }()
	go func() { defer wg.Done(); a.Subscriptions, errs[2] = c.Subscriptions(ctx) }()
	go func() { defer wg.Done(); a.Profile, errs[3] = c.Profile(ctx) }()
	wg.Wait()
	return &a, errors.Join(errs[:]...)
}
