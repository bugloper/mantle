package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// Output formatting. R-02 identifies "it's just a CLI" as the main adoption
// risk of shipping without a web interface, and names the mitigation: make the
// CLI's output genuinely good. So this is a real concern rather than
// decoration — the terminal is the entire product surface until `mantle-ui`
// exists.

// colour codes, blanked when the terminal cannot use them.
var (
	colourReset  = "\033[0m"
	colourBold   = "\033[1m"
	colourDim    = "\033[2m"
	colourGreen  = "\033[32m"
	colourYellow = "\033[33m"
	colourRed    = "\033[31m"
	colourCyan   = "\033[36m"
)

func init() {
	if !colourEnabled() {
		colourReset, colourBold, colourDim = "", "", ""
		colourGreen, colourYellow, colourRed, colourCyan = "", "", "", ""
	}
}

// colourEnabled honours NO_COLOR and detects a non-terminal, so that piping
// output into a file or a log does not fill it with escape sequences.
func colourEnabled() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	info, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func bold(s string) string   { return colourBold + s + colourReset }
func dim(s string) string    { return colourDim + s + colourReset }
func green(s string) string  { return colourGreen + s + colourReset }
func yellow(s string) string { return colourYellow + s + colourReset }
func red(s string) string    { return colourRed + s + colourReset }
func cyan(s string) string   { return colourCyan + s + colourReset }

// table renders aligned columns.
type table struct {
	headers []string
	rows    [][]string
}

func newTable(headers ...string) *table {
	return &table{headers: headers}
}

func (t *table) add(cells ...string) {
	t.rows = append(t.rows, cells)
}

// render writes the table, sizing columns to their widest cell.
//
// Width is measured in runes rather than bytes, so a repository name or a
// display name containing non-ASCII does not throw the alignment out.
func (t *table) render(w io.Writer) {
	if len(t.rows) == 0 {
		fmt.Fprintln(w, dim("  (none)"))
		return
	}

	widths := make([]int, len(t.headers))
	for i, header := range t.headers {
		widths[i] = utf8.RuneCountInString(header)
	}
	for _, row := range t.rows {
		for i, cell := range row {
			if i >= len(widths) {
				break
			}
			if n := utf8.RuneCountInString(cell); n > widths[i] {
				widths[i] = n
			}
		}
	}

	var header strings.Builder
	for i, h := range t.headers {
		header.WriteString(pad(strings.ToUpper(h), widths[i]))
		if i < len(t.headers)-1 {
			header.WriteString("  ")
		}
	}
	fmt.Fprintln(w, dim(strings.TrimRight(header.String(), " ")))

	for _, row := range t.rows {
		var line strings.Builder
		for i := range t.headers {
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			line.WriteString(pad(cell, widths[i]))
			if i < len(t.headers)-1 {
				line.WriteString("  ")
			}
		}
		fmt.Fprintln(w, strings.TrimRight(line.String(), " "))
	}
}

// pad right-pads to a rune width, ignoring any colour escapes already present.
func pad(s string, width int) string {
	n := utf8.RuneCountInString(stripColour(s))
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}

// stripColour removes ANSI escapes so width calculations see printable text.
func stripColour(s string) string {
	var out strings.Builder
	inEscape := false
	for _, r := range s {
		switch {
		case r == '\033':
			inEscape = true
		case inEscape && r == 'm':
			inEscape = false
		case !inEscape:
			out.WriteRune(r)
		}
	}
	return out.String()
}

// printJSON writes a value as indented JSON, which is what --json produces
// everywhere.
func printJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// humanBytes renders a byte count for people.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// humanAge renders a duration since a timestamp, at the granularity a person
// actually wants: "3h ago", not "3h14m22.041s ago".
func humanAge(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}

// shortDigest abbreviates a digest for display, keeping enough to be
// recognisable and identifiable by eye.
func shortDigest(digest string) string {
	_, encoded, found := strings.Cut(digest, ":")
	if !found {
		encoded = digest
	}
	if len(encoded) > 12 {
		return encoded[:12]
	}
	return encoded
}

// shortCommit abbreviates a git SHA the way git itself does.
func shortCommit(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// section prints a titled block heading.
func section(w io.Writer, title string) {
	fmt.Fprintf(w, "\n%s\n", bold(title))
}

// statusMark renders a pass/warn/fail glyph.
func statusMark(ok bool) string {
	if ok {
		return green("✓")
	}
	return red("✗")
}
