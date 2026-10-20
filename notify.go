package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Placeholders accepted in a --notify command template, mapped to the
// environment variable carrying the value.
var notifyVars = map[string]string{
	"title":       "BAUDUU_TITLE",
	"description": "BAUDUU_DESCRIPTION",
	"desc":        "BAUDUU_DESCRIPTION",
}

type quoteState int

const (
	quoteNone quoteState = iota
	quoteSingle
	quoteDouble
)

// frame is one shell parsing context: the top level, a $(...) or a `...`.
// Each starts unquoted, whatever quotes surround it.
type frame struct {
	state    quoteState
	backtick bool
	// parens counts open "(" inside a $(...) so the matching ")" ends it.
	parens int
}

// varRef returns a reference to env var name that expands to exactly its
// value in the given quoting context: bare -> "$X", inside "..." -> ${X},
// inside '...' -> '"$X"' (close, quoted expansion, reopen).
func varRef(name string, state quoteState) string {
	switch state {
	case quoteSingle:
		return `'"$` + name + `"'`
	case quoteDouble:
		return "${" + name + "}"
	default:
		return `"$` + name + `"`
	}
}

// renderCommand replaces {{title}} and {{description}} in a POSIX shell
// command template with references to BAUDUU_TITLE and BAUDUU_DESCRIPTION.
// The values themselves never become part of the command text, so the shell
// can not interpret them as syntax, wherever the placeholder stands. The
// reference is quoted for its context (bare, "double", 'single', also inside
// $(...) and backticks), so the value also reaches the program as a single
// unsplit argument. Unknown placeholders and unbalanced quotes are errors, so
// a typo does not silently send a broken notification.
func renderCommand(tmpl string) (string, error) {
	var b strings.Builder
	stack := []frame{{}}
	for i := 0; i < len(tmpl); i++ {
		top := &stack[len(stack)-1]
		if strings.HasPrefix(tmpl[i:], "{{") {
			if end := strings.Index(tmpl[i+2:], "}}"); end >= 0 {
				name := strings.TrimSpace(tmpl[i+2 : i+2+end])
				env, ok := notifyVars[strings.ToLower(name)]
				if !ok {
					return "", fmt.Errorf("unknown placeholder {{%s}} (use {{title}} or {{description}})", name)
				}
				b.WriteString(varRef(env, top.state))
				i += 2 + end + 1
				continue
			}
		}
		c := tmpl[i]
		b.WriteByte(c)
		switch top.state {
		case quoteSingle:
			if c == '\'' {
				top.state = quoteNone
			}
			continue
		case quoteDouble:
			switch {
			case c == '\\' && i+1 < len(tmpl):
				i++
				b.WriteByte(tmpl[i])
			case c == '"':
				top.state = quoteNone
			case c == '$' && strings.HasPrefix(tmpl[i+1:], "("):
				i++
				b.WriteByte('(')
				stack = append(stack, frame{})
			case c == '`':
				stack = append(stack, frame{backtick: true})
			}
			continue
		}
		switch {
		case c == '\\' && i+1 < len(tmpl):
			// An escaped character never changes the parsing state.
			i++
			b.WriteByte(tmpl[i])
		case c == '#' && wordStart(tmpl, i):
			// A comment runs to the end of the line; quotes, parentheses and
			// placeholders in it mean nothing to the shell, so copy it as is.
			end := strings.IndexByte(tmpl[i:], '\n')
			if end < 0 {
				end = len(tmpl) - i
			}
			b.WriteString(tmpl[i+1 : i+end])
			i += end - 1
		case c == '\'':
			top.state = quoteSingle
		case c == '"':
			top.state = quoteDouble
		case c == '$' && strings.HasPrefix(tmpl[i+1:], "("):
			i++
			b.WriteByte('(')
			stack = append(stack, frame{})
		case c == '(':
			top.parens++
		case c == ')' && top.parens > 0:
			top.parens--
		case c == ')' && len(stack) > 1 && !top.backtick:
			stack = stack[:len(stack)-1]
		case c == '`' && top.backtick:
			stack = stack[:len(stack)-1]
		case c == '`':
			stack = append(stack, frame{backtick: true})
		}
	}
	if len(stack) != 1 || stack[0].state != quoteNone {
		return "", fmt.Errorf("unbalanced quotes or substitutions in notify command %q", tmpl)
	}
	return b.String(), nil
}

// wordStart reports whether tmpl[i] begins a shell word, which is where
// "#" starts a comment.
func wordStart(tmpl string, i int) bool {
	return i == 0 || strings.IndexByte(" \t\n;&|()<>", tmpl[i-1]) >= 0
}

// runNotify renders the template and runs it with /bin/sh, passing the
// values through the environment.
func runNotify(ctx context.Context, tmpl, title, description string) error {
	cmdline, err := renderCommand(tmpl)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", cmdline)
	cmd.Env = append(os.Environ(), "BAUDUU_TITLE="+title, "BAUDUU_DESCRIPTION="+description)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("notify command failed: %w", err)
	}
	return nil
}

// shellQuote single-quotes s for display in a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, `'`, `'\''`) + "'"
}
