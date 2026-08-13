package picker

import (
	"strings"
	"testing"
)

func items() []Item {
	return []Item{
		{Label: "kubernetescluster", Columns: []string{"kubernetescluster", "kubernetesclusters"}},
		{Label: "pod", Columns: []string{"pod", "pods"}},
		{Label: "vulnerabilityreport", Columns: []string{"vulnerabilityreport", "vulnerabilityreports"}},
		{Label: "namespace", Columns: []string{"namespace", "namespaces"}},
	}
}

func visibleLabels(m *model) []string {
	out := make([]string, 0, len(m.visible()))
	for _, it := range m.visible() {
		out = append(out, it.Label)
	}
	return out
}

func TestNewModelShowsEverythingWithTheFirstRowSelected(t *testing.T) {
	m := newModel(items())

	if got := len(m.visible()); got != 4 {
		t.Errorf("visible() = %d items, want all 4", got)
	}
	sel, ok := m.selected()
	if !ok {
		t.Fatal("selected() = not ok, want the first row")
	}
	if sel.Label != "kubernetescluster" {
		t.Errorf("selected() = %q, want the first item", sel.Label)
	}
}

func TestTypingFiltersFuzzily(t *testing.T) {
	m := newModel(items())

	m.push('p')
	m.push('o')
	m.push('d')

	got := visibleLabels(m)
	if len(got) == 0 {
		t.Fatal("query \"pod\" matched nothing")
	}
	if got[0] != "pod" {
		t.Errorf("best match = %q, want pod (got %v)", got[0], got)
	}
	for _, label := range got {
		if !strings.Contains(label, "p") {
			t.Errorf("%q does not plausibly match \"pod\"", label)
		}
	}
}

func TestBackspaceRestoresEarlierMatches(t *testing.T) {
	m := newModel(items())
	for _, r := range "pod" {
		m.push(r)
	}
	narrowed := len(m.visible())

	m.backspace()
	m.backspace()
	m.backspace()

	if m.query != "" {
		t.Errorf("query = %q after backspacing it away, want empty", m.query)
	}
	if got := len(m.visible()); got != 4 {
		t.Errorf("visible() = %d after clearing the query (was %d narrowed), want all 4", got, narrowed)
	}
}

func TestBackspaceOnEmptyQueryIsHarmless(t *testing.T) {
	m := newModel(items())
	m.backspace()
	if m.query != "" || len(m.visible()) != 4 {
		t.Errorf("backspace on an empty query changed state: query=%q visible=%d", m.query, len(m.visible()))
	}
}

func TestClearResetsTheQuery(t *testing.T) {
	m := newModel(items())
	for _, r := range "vuln" {
		m.push(r)
	}
	m.clear()

	if m.query != "" {
		t.Errorf("query = %q after clear(), want empty", m.query)
	}
	if len(m.visible()) != 4 {
		t.Errorf("visible() = %d after clear(), want all 4", len(m.visible()))
	}
}

func TestCursorMovesAndClampsAtBothEnds(t *testing.T) {
	m := newModel(items())

	m.up() // already at the top
	if m.cursor != 0 {
		t.Errorf("cursor = %d after up() at the top, want 0", m.cursor)
	}

	for range 10 {
		m.down()
	}
	if want := len(m.visible()) - 1; m.cursor != want {
		t.Errorf("cursor = %d after running past the bottom, want %d", m.cursor, want)
	}

	m.up()
	if want := len(m.visible()) - 2; m.cursor != want {
		t.Errorf("cursor = %d after up(), want %d", m.cursor, want)
	}
}

// A filter that shortens the list must not leave the cursor pointing past the
// end — that would select the wrong item or none at all.
func TestFilteringClampsTheCursor(t *testing.T) {
	m := newModel(items())
	for range 3 {
		m.down()
	}
	if m.cursor != 3 {
		t.Fatalf("cursor = %d, want 3 before filtering", m.cursor)
	}

	for _, r := range "pod" {
		m.push(r)
	}

	if m.cursor >= len(m.visible()) {
		t.Errorf("cursor = %d with only %d visible rows", m.cursor, len(m.visible()))
	}
	sel, ok := m.selected()
	if !ok {
		t.Fatal("selected() = not ok after filtering")
	}
	if sel.Label != m.visible()[m.cursor].Label {
		t.Errorf("selected() = %q, but cursor points at %q", sel.Label, m.visible()[m.cursor].Label)
	}
}

func TestSelectedIsNotOkWhenNothingMatches(t *testing.T) {
	m := newModel(items())
	for _, r := range "zzzznotakind" {
		m.push(r)
	}

	if len(m.visible()) != 0 {
		t.Fatalf("visible() = %v, want no matches", visibleLabels(m))
	}
	if _, ok := m.selected(); ok {
		t.Error("selected() = ok with no matches, want not ok")
	}
}

func TestPagingMovesByViewport(t *testing.T) {
	many := make([]Item, 30)
	for i := range many {
		many[i] = Item{Label: string(rune('a'+i%26)) + "-item"}
	}
	m := newModel(many)
	m.viewport = 10

	m.pageDown()
	if m.cursor != 10 {
		t.Errorf("cursor = %d after pageDown with a viewport of 10, want 10", m.cursor)
	}
	m.pageUp()
	if m.cursor != 0 {
		t.Errorf("cursor = %d after pageUp, want 0", m.cursor)
	}
	m.pageUp()
	if m.cursor != 0 {
		t.Errorf("cursor = %d after pageUp at the top, want it clamped to 0", m.cursor)
	}
}

// The header must be measured with the data, or the title row drifts out of
// alignment with the rows beneath it.
func TestHeaderAlignsWithRows(t *testing.T) {
	m := newModel([]Item{
		{Label: "a", Columns: []string{"a", "short"}},
		{Label: "b", Columns: []string{"bbbbbbbbbb", "x"}},
	}).withHeader(Item{Columns: []string{"KIND", "PLURAL"}})

	header := m.headerRow()
	rows := m.rows()

	want := strings.Index(header, "PLURAL")
	if want < 0 {
		t.Fatalf("header %q has no second column", header)
	}
	for _, row := range rows {
		cell := strings.TrimSpace(row[strings.Index(row, " "):])
		if got := strings.Index(row, cell); got != want {
			t.Errorf("row %q starts its second column at %d, header at %d", row, got, want)
		}
	}
}

// A header wider than any data cell must widen the column, not overflow it.
func TestHeaderWiderThanDataStillAligns(t *testing.T) {
	m := newModel([]Item{
		{Label: "a", Columns: []string{"a", "x"}},
	}).withHeader(Item{Columns: []string{"AVERYLONGHEADER", "PLURAL"}})

	header := m.headerRow()
	row := m.rows()[0]
	if strings.Index(header, "PLURAL") != strings.Index(row, "x") {
		t.Errorf("columns misaligned when the header is the widest cell:\n%q\n%q", header, row)
	}
}

// Rows are padded per column so the picker lines up like the CLI's tables.
func TestRowsAreColumnAligned(t *testing.T) {
	m := newModel([]Item{
		{Label: "a", Columns: []string{"a", "short"}},
		{Label: "b", Columns: []string{"bbbbbbbbbb", "x"}},
	})

	rows := m.rows()
	if len(rows) != 2 {
		t.Fatalf("rows() = %d, want 2", len(rows))
	}
	first := strings.Index(rows[0], "short")
	second := strings.Index(rows[1], "x")
	if first != second {
		t.Errorf("second column starts at %d and %d; columns are not aligned:\n%q\n%q",
			first, second, rows[0], rows[1])
	}
}
