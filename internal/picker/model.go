// Package picker presents an interactive single-select list in the terminal.
//
// The state machine lives in model, separate from any terminal I/O, so the
// interesting behaviour — fuzzy filtering, cursor movement, and which item a
// selection actually resolves to — is testable without a TTY. Select adds the
// termui rendering on top.
package picker

import (
	"strings"
	"unicode/utf8"

	"github.com/sahilm/fuzzy"
)

// Item is one selectable row.
type Item struct {
	// Label is what the query is fuzzy-matched against. Include everything
	// worth searching on, not only what is displayed.
	Label string
	// Columns are the cell values rendered for this row, padded into
	// alignment with the other rows.
	Columns []string
	// Value is the caller's own payload for the row.
	Value any
}

// defaultViewport is the assumed page size before a terminal height is known.
const defaultViewport = 10

// model holds the picker's state: the full item set, the current query, the
// filtered view, and where the cursor sits within it.
type model struct {
	all      []Item
	filtered []Item
	query    string
	cursor   int
	viewport int
	// header is measured alongside the data when sizing columns, so the
	// title row lines up with the rows beneath it.
	header Item
}

func newModel(all []Item) *model {
	m := &model{all: all, viewport: defaultViewport}
	m.applyFilter()
	return m
}

// withHeader returns the model set up to reserve width for a header row.
func (m *model) withHeader(header Item) *model {
	m.header = header
	return m
}

// headerRow renders the column titles at the same widths as the data rows.
func (m *model) headerRow() string {
	return renderRow(m.header, m.columnWidths())
}

// visible returns the rows currently matching the query.
func (m *model) visible() []Item { return m.filtered }

// selected returns the item under the cursor. It reports false when the query
// matches nothing, so callers never select from an empty list.
func (m *model) selected() (Item, bool) {
	if m.cursor < 0 || m.cursor >= len(m.filtered) {
		return Item{}, false
	}
	return m.filtered[m.cursor], true
}

// push appends a rune to the query and refilters.
func (m *model) push(r rune) {
	m.query += string(r)
	m.applyFilter()
}

// backspace removes the last rune of the query, if any.
func (m *model) backspace() {
	if n := utf8.RuneCountInString(m.query); n > 0 {
		runes := []rune(m.query)
		m.query = string(runes[:n-1])
		m.applyFilter()
	}
}

// clear empties the query.
func (m *model) clear() {
	m.query = ""
	m.applyFilter()
}

// applyFilter recomputes the visible rows and keeps the cursor in range —
// a shorter list must never leave it pointing past the end.
func (m *model) applyFilter() {
	if m.query == "" {
		m.filtered = m.all
	} else {
		labels := make([]string, len(m.all))
		for i, it := range m.all {
			labels[i] = it.Label
		}
		matches := fuzzy.Find(m.query, labels)
		out := make([]Item, 0, len(matches))
		for _, match := range matches {
			out = append(out, m.all[match.Index])
		}
		m.filtered = out
	}
	m.clampCursor()
}

func (m *model) clampCursor() {
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func (m *model) up()   { m.moveBy(-1) }
func (m *model) down() { m.moveBy(1) }

func (m *model) pageUp()   { m.moveBy(-m.viewport) }
func (m *model) pageDown() { m.moveBy(m.viewport) }

func (m *model) moveBy(delta int) {
	m.cursor += delta
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

// rows renders the visible items as column-aligned strings, so the picker
// reads like the tables the rest of the CLI prints.
func (m *model) rows() []string {
	widths := m.columnWidths()
	out := make([]string, 0, len(m.filtered))
	for _, it := range m.filtered {
		out = append(out, renderRow(it, widths))
	}
	return out
}

// columnWidths measures each column across the visible rows and the header,
// so both render at the same widths.
func (m *model) columnWidths() []int {
	var widths []int
	measure := func(it Item) {
		for i, cell := range it.Columns {
			w := len([]rune(cell))
			if i >= len(widths) {
				widths = append(widths, w)
				continue
			}
			if w > widths[i] {
				widths[i] = w
			}
		}
	}
	measure(m.header)
	for _, it := range m.filtered {
		measure(it)
	}
	return widths
}

// renderRow pads an item's cells to the given column widths. The trailing
// column is not padded, so rows carry no trailing whitespace.
func renderRow(it Item, widths []int) string {
	var b strings.Builder
	for i, cell := range it.Columns {
		b.WriteString(cell)
		if i < len(it.Columns)-1 && i < len(widths) {
			b.WriteString(strings.Repeat(" ", widths[i]-len([]rune(cell))+2))
		}
	}
	return strings.TrimRight(b.String(), " ")
}
