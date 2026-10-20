package bauduu

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSite serves the fixtures like the real site: customer pages need the
// session cookie set by a successful login, otherwise the login page comes
// back with HTTP 200.
func fakeSite(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var productHits atomic.Int32
	serve := func(w http.ResponseWriter, name string) {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Error(err)
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
	}
	authed := func(r *http.Request) bool {
		c, err := r.Cookie("sess")
		return err == nil && c.Value == "ok"
	}
	pages := map[string]string{
		"/lego/login/profile": "profile.html",
		"/lego/ausleihliste":  "ausleihliste.html",
		"/lego/meine-sets":    "meine_sets.html",
		"/lego/my-dashboard":  "dashboard.html",
		"/lego/myuser":        "myuser.html",
		"/lego/retour":        "retour.html",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/lego/login" && r.Method == http.MethodPost:
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.PostForm.Get(strings.Repeat("0", 32)) != "1" || r.PostForm.Get("task") != "user.login" {
				t.Errorf("login form missing token/task: %v", r.PostForm)
			}
			if r.PostForm.Get("username") == "erika" && r.PostForm.Get("password") == "s3cret" {
				http.SetCookie(w, &http.Cookie{Name: "sess", Value: "ok", Path: "/"})
				serve(w, "profile.html")
				return
			}
			serve(w, "login_failed.html")
		case r.URL.Path == "/lego/login":
			serve(w, "login.html")
		case strings.HasPrefix(r.URL.Path, "/lego/themenwelt/"):
			productHits.Add(1)
			serve(w, "product.html")
		default:
			page, ok := pages[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if !authed(r) {
				serve(w, "login.html")
				return
			}
			serve(w, page)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &productHits
}

func TestLoginFailure(t *testing.T) {
	srv, _ := fakeSite(t)
	c, err := New(srv.URL+"/lego", 5*time.Second, "test")
	if err != nil {
		t.Fatal(err)
	}
	err = c.Login(context.Background(), "erika", "wrong")
	var le *LoginError
	if !errors.As(err, &le) || !strings.Contains(le.Message, "Passwort falsch") {
		t.Fatalf("Login error = %v", err)
	}
}

func TestNotLoggedIn(t *testing.T) {
	srv, _ := fakeSite(t)
	c, _ := New(srv.URL+"/lego/", 5*time.Second, "test")
	if _, err := c.Loans(context.Background()); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("Loans without login = %v, want ErrNotLoggedIn", err)
	}
}

func TestAccount(t *testing.T) {
	srv, productHits := fakeSite(t)
	c, _ := New(srv.URL+"/lego/", 5*time.Second, "test")
	ctx := context.Background()
	if err := c.Login(ctx, "erika", "s3cret"); err != nil {
		t.Fatal(err)
	}
	a, err := c.Account(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Wishlist) != 11 || len(a.Loans) != 11 || len(a.Subscriptions) != 3 || a.Profile == nil {
		t.Fatalf("account = %d/%d/%d/%v", len(a.Wishlist), len(a.Loans), len(a.Subscriptions), a.Profile)
	}
	if got := productHits.Load(); got != 11 {
		t.Errorf("product pages fetched = %d, want 11", got)
	}
	// Every product page is the same fixture, so every item gets its fields.
	if a.Wishlist[3].Package != "PROFI" || a.Wishlist[3].SKU != "60124-1" {
		t.Errorf("details not applied: %+v", a.Wishlist[3])
	}
	if !strings.HasPrefix(a.Wishlist[0].URL, srv.URL+"/lego/themenwelt/") {
		t.Errorf("wishlist URL not resolved against base: %s", a.Wishlist[0].URL)
	}
	if a.Profile.CustomerNumber != "10001" || a.Profile.UserID != 20002 {
		t.Errorf("profile = %+v", a.Profile)
	}
	if n, err := c.CustomerNumber(ctx); err != nil || n != "10001" {
		t.Errorf("CustomerNumber = %q, %v", n, err)
	}
}

func TestWishlistDetailFailureIsWarning(t *testing.T) {
	srv, _ := fakeSite(t)
	c, _ := New(srv.URL+"/lego/", 5*time.Second, "test")
	ctx := context.Background()
	if err := c.Login(ctx, "erika", "s3cret"); err != nil {
		t.Fatal(err)
	}
	var warnings atomic.Int32
	c.Warn = func(error) { warnings.Add(1) }
	srv.Config.Handler = wrapProducts(srv.Config.Handler)
	items, err := c.Wishlist(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 11 || warnings.Load() != 11 {
		t.Fatalf("items=%d warnings=%d", len(items), warnings.Load())
	}
	if items[0].Package != "" {
		t.Errorf("failed details should leave item untouched: %+v", items[0])
	}
}

// wrapProducts makes every product page fail.
func wrapProducts(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/lego/themenwelt/") {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func TestWishlistDetailsNeedSession(t *testing.T) {
	srv, _ := fakeSite(t)
	c, _ := New(srv.URL+"/lego/", 5*time.Second, "test")
	ctx := context.Background()
	if err := c.Login(ctx, "erika", "s3cret"); err != nil {
		t.Fatal(err)
	}
	var warnings []error
	var mu sync.Mutex
	c.Warn = func(err error) { mu.Lock(); warnings = append(warnings, err); mu.Unlock() }
	// Session expires after the wishlist: product pages come back anonymous.
	next := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/lego/themenwelt/") {
			b, _ := os.ReadFile(filepath.Join("testdata", "login.html"))
			_, _ = w.Write(b)
			return
		}
		next.ServeHTTP(w, r)
	})
	items, err := c.Wishlist(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != len(items) || !errors.Is(warnings[0], ErrNotLoggedIn) {
		t.Fatalf("warnings = %v", warnings)
	}
	if items[0].Package != "" || items[0].SKU != "" {
		t.Errorf("anonymous page must not be applied: %+v", items[0])
	}
}
