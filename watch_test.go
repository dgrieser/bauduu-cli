package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dgrieser/bauduu-cli/internal/bauduu"
)

const nastyValue = "it's \"quoted\" $HOME `id` $(id) \\ ; & | * % {{title}}\nline two"

// runRendered executes a template like runNotify, capturing stdout.
func runRendered(t *testing.T, tmpl, title, description string) string {
	t.Helper()
	cmdline, err := renderCommand(tmpl)
	if err != nil {
		t.Fatalf("%s: %v", tmpl, err)
	}
	cmd := exec.Command("/bin/sh", "-c", cmdline)
	cmd.Env = append(os.Environ(), "BAUDUU_TITLE="+title, "BAUDUU_DESCRIPTION="+description)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: run %q: %v", tmpl, cmdline, err)
	}
	return string(out)
}

// The program must receive the exact value as one argument, whatever
// quoting or substitution context the placeholder sits in.
func TestRenderCommandRoundTrip(t *testing.T) {
	cases := []struct{ tmpl, want string }{
		{`printf '%s' {{description}}`, nastyValue},
		{`printf '%s' "{{description}}"`, nastyValue},
		{`printf '%s' '{{description}}'`, nastyValue},
		{`printf '%s' "prefix {{description}} suffix"`, "prefix " + nastyValue + " suffix"},
		{`printf '%s' 'prefix {{ desc }} suffix'`, "prefix " + nastyValue + " suffix"},
		{`printf '%s' x\"{{description}}`, `x"` + nastyValue},
		{`printf '%s' "$(printf '%s' {{description}})"`, nastyValue},
		{`printf '%s' "$(printf '%s' "$(printf '%s' {{description}})")"`, nastyValue},
		{`printf '%s' "$( (printf '%s' {{description}}) )"`, nastyValue},
		{"printf '%s' \"`printf '%s' {{description}}`\"", nastyValue},
		{`printf '%s|%s' {{title}} {{description}}`, "T 'x'|" + nastyValue},
		{"printf '%s' {{description}} # don't retry (or \"do\"", nastyValue},
		{"printf '%s' \"$(printf '%s' {{description}} # it's ) here\n)\"", nastyValue},
		{`printf '%s' a#{{title}}`, "a#T 'x'"},
	}
	for _, c := range cases {
		if got := runRendered(t, c.tmpl, "T 'x'", nastyValue); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.tmpl, got, c.want)
		}
	}
}

// Even where the context is not tracked (here-documents, comments), the
// value must never execute: it is only ever expanded, not parsed.
func TestRenderCommandNeverExecutesValue(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pwned")
	evil := "x\n$(touch " + marker + ")\n`touch " + marker + "`\ntouch " + marker + "\n"
	for _, tmpl := range []string{
		"cat <<EOF\n{{description}}\nEOF",
		"true # {{description}}",
		`echo $(( 1 )) {{description}} >/dev/null`,
	} {
		runRendered(t, tmpl, "t", evil)
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%q executed the value", tmpl)
		}
	}
}

func TestRenderCommandErrors(t *testing.T) {
	for _, tmpl := range []string{
		`notify {{titel}}`,
		`notify "{{title}}`,
		`notify '{{title}}`,
		`notify "$(echo {{title}}"`,
		"notify `echo {{title}}",
	} {
		if _, err := renderCommand(tmpl); err == nil {
			t.Errorf("%s: expected error", tmpl)
		}
	}
	// Text that merely contains braces stays as it is.
	if got, err := renderCommand(`echo {a,b} {{`); err != nil || got != `echo {a,b} {{` {
		t.Errorf("braces: %q, %v", got, err)
	}
}

func TestRunNotify(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	tmpl := `printf '%s|%s' {{title}} {{description}} > ` + shellQuote(out)
	if err := runNotify(context.Background(), tmpl, "T 'x'", nastyValue); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if string(b) != "T 'x'|"+nastyValue {
		t.Errorf("values = %q", b)
	}
	if err := runNotify(context.Background(), "exit 3", "t", "d"); err == nil {
		t.Error("failing command must return an error")
	}
}

func day(s string) bauduu.Date {
	t, _ := time.Parse(time.DateOnly, s)
	return bauduu.Date{Time: t}
}

func TestDiffLoans(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)
	old := []bauduu.Loan{
		{ID: 5, SKU: "60036-1", Name: "Arktis Basislager 60036", Shipped: day("2026-04-28"), Status: "Ausgeliehen"},
		{ID: 4, SKU: "60228-1", Name: "Weltraumrakete 60228", Shipped: day("2026-03-16"), Returned: day("2026-04-27"), Status: "Zurückgegeben"},
		{ID: 1, SKU: "10000-1", Name: "Altes Set 10000", Shipped: day("2025-10-01"), Returned: day("2025-10-20"), Status: "Zurückgegeben"},
	}
	cur := []bauduu.Loan{
		{ID: 7, SKU: "31119-1", Name: "Riesenrad 3in1 31119", Shipped: day("2026-10-14"), Status: "Ausgeliehen"},
		{ID: 6, SKU: "W", Name: "Nächster Versand ab 2026-06-16", Shipped: day("2026-06-16"), Returned: day("2026-06-16"), Status: "Zurückgegeben", Placeholder: true},
		{ID: 5, SKU: "60036-1", Name: "Arktis Basislager 60036", Shipped: day("2026-04-28"), Returned: day("2026-06-09"), Status: "Zurückgegeben", BonusBricks: 3},
		{ID: 4, SKU: "60228-1", Name: "Weltraumrakete 60228", Shipped: day("2026-03-16"), Returned: day("2026-04-27"), Status: "Zurückgegeben", BonusBricks: 3},
	}
	got := diffLoans(old, cur, now)
	want := []loanChange{
		{"Neues Set unterwegs: Riesenrad 3in1", "Versendet: Riesenrad 3in1 (31119) am 2026-10-14"},
		{"Versand geplant", "Nächster Versand ab 2026-06-16"},
		{"Retour verbucht: Arktis Basislager", "Retour verbucht: Arktis Basislager (60036) am 2026-06-09 nach 42 Tagen; Bonussteine Arktis Basislager (60036): +3 (jetzt 3)"},
		{"Bonussteine: Weltraumrakete", "Bonussteine Weltraumrakete (60228): +3 (jetzt 3)"},
		{"Eintrag entfernt: Altes Set", "Entfernt: Altes Set (10000)"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d changes: %+v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("change %d:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
	if len(diffLoans(cur, cur, now)) != 0 {
		t.Error("identical lists must not differ")
	}

	title, desc := summarize(got[:1])
	if title != "Bauduu: Neues Set unterwegs: Riesenrad 3in1" || desc != want[0].Line {
		t.Errorf("summarize one = %q / %q", title, desc)
	}
	title, desc = summarize(got)
	if title != "Bauduu: 5 Änderungen bei Bisherige Sets" || strings.Count(desc, "\n") != 4 {
		t.Errorf("summarize many = %q / %q", title, desc)
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state.json")
	if s, err := loadState(path); err != nil || s != nil {
		t.Fatalf("missing state = %v, %v", s, err)
	}
	loans := testLoans()
	in := &watchState{User: "erika", Checked: time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC), Loans: loans}
	if err := saveState(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if out.User != "erika" || !out.Checked.Equal(in.Checked) || len(diffLoans(loans, out.Loans, time.Now())) != 0 {
		t.Errorf("round trip changed state: %+v", out)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("temp file left behind: %v", entries)
	}
}

func TestDefaultStatePath(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/state")
	got := defaultStatePath("Erika/Muster x")
	if !strings.HasPrefix(got, "/state/bauduu/sets-erika_muster_x-") || !strings.HasSuffix(got, ".json") {
		t.Errorf("defaultStatePath = %q", got)
	}
	if defaultStatePath("a+b") == defaultStatePath("a@b") {
		t.Error("distinct users must not share a state file")
	}
	if defaultStatePath("Erika") != defaultStatePath("erika") {
		t.Error("usernames are case-insensitive on the site")
	}
}

func TestWatchTestNotification(t *testing.T) {
	var buf bytes.Buffer
	w := &watchOptions{notify: `notify -t {{title}} -d {{description}}`, test: true, dryRun: true}
	if err := runWatch(context.Background(), &buf, &options{}, w); err != nil {
		t.Fatal(err)
	}
	want := `BAUDUU_TITLE='Bauduu: Testbenachrichtigung' BAUDUU_DESCRIPTION='Quoting-Test: '\''einfach'\''`
	if !strings.HasPrefix(buf.String(), want) || !strings.HasSuffix(buf.String(), ` /bin/sh -c 'notify -t "$BAUDUU_TITLE" -d "$BAUDUU_DESCRIPTION"'`+"\n") {
		t.Errorf("dry-run output = %q", buf.String())
	}
	if err := runWatch(context.Background(), &buf, &options{}, &watchOptions{notify: "x {{nope}}"}); err == nil {
		t.Error("bad template must fail before any network access")
	}
}
