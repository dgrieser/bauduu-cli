package main

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/mattn/go-runewidth"
)

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

type align int

const (
	alignLeft align = iota
	alignRight
)

type column struct {
	Title string
	Align align
	// Flex columns shrink first when the table is wider than the terminal;
	// their cells then wrap at word boundaries.
	Flex bool
	// MinWidth bounds how far a flex column may shrink.
	MinWidth int
	// Drop hides the column when the table still does not fit after all flex
	// columns reached their minimum: 1 goes first, then 2, ...; 0 never.
	Drop int
}

type row struct {
	Cells []string
	// Style is an ANSI sequence applied to the whole row (e.g. dim).
	Style string
	// CellStyles override Style per cell; missing or empty entries fall back.
	CellStyles []string
}

type table struct {
	Title   string
	Columns []column
	Rows    []row
	Footer  []string
}

func (t *table) add(cells ...string) *row {
	t.Rows = append(t.Rows, row{Cells: cells})
	return &t.Rows[len(t.Rows)-1]
}

func (t *table) cell(r row, i int) string {
	if i < len(r.Cells) {
		return r.Cells[i]
	}
	return ""
}

// naturalWidths returns each column's widest line, title included.
func (t *table) naturalWidths() []int {
	w := make([]int, len(t.Columns))
	for i, c := range t.Columns {
		w[i] = runewidth.StringWidth(c.Title)
		for _, r := range t.Rows {
			for _, line := range strings.Split(t.cell(r, i), "\n") {
				w[i] = max(w[i], runewidth.StringWidth(line))
			}
		}
	}
	return w
}

func (t *table) minWidth(i int) int {
	c := t.Columns[i]
	return max(c.MinWidth, runewidth.StringWidth(c.Title), 4)
}

// layout picks the visible columns (by index) and their widths for maxWidth
// terminal columns; maxWidth <= 0 means unlimited. If even the minimal layout
// is too wide, the table overflows rather than losing more columns.
func (t *table) layout(maxWidth int) (visible, widths []int) {
	natural := t.naturalWidths()
	for i := range t.Columns {
		visible = append(visible, i)
	}
	for {
		widths = make([]int, len(visible))
		// Border overhead: "│ " + " │ " between columns + " │".
		total := 3*len(visible) + 1
		for j, i := range visible {
			widths[j] = natural[i]
			total += widths[j]
		}
		if maxWidth <= 0 {
			return visible, widths
		}
		// Shrink the widest flex column one step at a time so the space is
		// shared instead of one column taking all the cuts.
		for total > maxWidth {
			best := -1
			for j, i := range visible {
				if t.Columns[i].Flex && widths[j] > t.minWidth(i) && (best < 0 || widths[j] > widths[best]) {
					best = j
				}
			}
			if best < 0 {
				break
			}
			widths[best]--
			total--
		}
		if total <= maxWidth {
			return visible, widths
		}
		drop := -1
		for j, i := range visible {
			if d := t.Columns[i].Drop; d > 0 && (drop < 0 || d < t.Columns[visible[drop]].Drop) {
				drop = j
			}
		}
		if drop < 0 {
			return visible, widths
		}
		visible = slices.Delete(visible, drop, drop+1)
	}
}

// wrap breaks s into lines of at most width display columns, at spaces where
// possible and inside over-long words otherwise.
func wrap(s string, width int) []string {
	var out []string
	for _, para := range strings.Split(s, "\n") {
		line := ""
		flush := func() {
			out = append(out, line)
			line = ""
		}
		for _, word := range strings.Fields(para) {
			for runewidth.StringWidth(word) > width {
				if line != "" {
					flush()
				}
				head := runewidth.Truncate(word, width, "")
				// Prefer breaking after a hyphen ("Feuerreifen-|Challenge").
				if i := strings.LastIndex(head, "-"); i > 0 {
					head = head[:i+1]
				}
				if head == "" {
					// A single rune wider than the column; emit it anyway.
					head = string([]rune(word)[:1])
				}
				out = append(out, head)
				word = word[len(head):]
			}
			switch {
			case word == "":
			case line == "":
				line = word
			case runewidth.StringWidth(line)+1+runewidth.StringWidth(word) <= width:
				line += " " + word
			default:
				flush()
				line = word
			}
		}
		flush()
	}
	return out
}

func (t *table) render(out io.Writer, color bool, maxWidth int) {
	style := func(s, code string) string {
		if !color || code == "" {
			return s
		}
		return code + s + ansiReset
	}
	if t.Title != "" {
		fmt.Fprintln(out, style(t.Title, ansiBold+ansiCyan))
	}
	if len(t.Rows) == 0 {
		// Keep the frame intact for an empty table.
		empty := *t
		empty.Rows = []row{{Cells: []string{"(keine Einträge)"}, Style: ansiDim}}
		t = &empty
	}
	visible, w := t.layout(maxWidth)
	line := func(left, mid, right string) {
		var b strings.Builder
		b.WriteString(left)
		for j, x := range w {
			if j > 0 {
				b.WriteString(mid)
			}
			b.WriteString(strings.Repeat("─", x+2))
		}
		b.WriteString(right)
		fmt.Fprintln(out, style(b.String(), ansiDim))
	}
	bar := style("│", ansiDim)
	writeRow := func(r row, header bool) {
		lines := make([][]string, len(visible))
		height := 1
		for j, i := range visible {
			text := t.cell(r, i)
			if header {
				text = t.Columns[i].Title
			}
			lines[j] = strings.Split(text, "\n")
			if t.Columns[i].Flex {
				lines[j] = wrap(text, w[j])
			}
			height = max(height, len(lines[j]))
		}
		for l := 0; l < height; l++ {
			var b strings.Builder
			b.WriteString(bar)
			for j, i := range visible {
				text := ""
				if l < len(lines[j]) {
					text = lines[j][l]
				}
				// Only reachable when the terminal is narrower than the
				// minimal layout; keep the frame aligned regardless.
				text = runewidth.Truncate(text, w[j], "…")
				pad := strings.Repeat(" ", w[j]-runewidth.StringWidth(text))
				if t.Columns[i].Align == alignRight && !header {
					text = pad + text
				} else {
					text += pad
				}
				code := r.Style
				if i < len(r.CellStyles) && r.CellStyles[i] != "" {
					code = r.CellStyles[i]
				}
				if header {
					code = ansiBold
				}
				b.WriteString(" " + style(text, code) + " " + bar)
			}
			fmt.Fprintln(out, b.String())
		}
	}
	line("┌", "┬", "┐")
	writeRow(row{}, true)
	line("├", "┼", "┤")
	for _, r := range t.Rows {
		writeRow(r, false)
	}
	line("└", "┴", "┘")
	for _, f := range t.Footer {
		fmt.Fprintln(out, style(f, ansiDim))
	}
	if len(visible) < len(t.Columns) {
		var hidden []string
		for i, c := range t.Columns {
			if !slices.Contains(visible, i) {
				hidden = append(hidden, c.Title)
			}
		}
		fmt.Fprintln(out, style("Ausgeblendet (Terminal zu schmal): "+strings.Join(hidden, ", ")+" · alles mit -o json", ansiDim))
	}
}
