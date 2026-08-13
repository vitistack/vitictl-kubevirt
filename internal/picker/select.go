package picker

import (
	"errors"
	"fmt"
	"os"
	"unicode/utf8"

	ui "github.com/gizak/termui/v3"
	"github.com/gizak/termui/v3/widgets"
	"golang.org/x/term"
)

// ErrCancelled is returned when the user dismisses the picker without
// choosing. Callers should treat it as "no selection", not as a failure.
var ErrCancelled = errors.New("selection cancelled")

// Interactive reports whether a picker can be shown. termui takes over the
// terminal, so it is only safe when the session is attached to one; a piped
// or CI invocation must be told to pass its argument explicitly instead.
func Interactive() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// Select shows items in a full-screen list with a fuzzy filter and returns the
// chosen one. It returns ErrCancelled if the user presses Esc, q, or Ctrl-C.
//
// The header row is drawn above the list; pass the column titles matching each
// Item's Columns.
func Select(title string, header []string, items []Item) (Item, error) {
	if len(items) == 0 {
		return Item{}, errors.New("nothing to choose from")
	}
	if err := ui.Init(); err != nil {
		return Item{}, fmt.Errorf("starting the terminal UI: %w", err)
	}
	defer ui.Close()

	m := newModel(items).withHeader(Item{Columns: header})

	list := widgets.NewList()
	list.Title = title
	list.TitleStyle = ui.NewStyle(ui.ColorGreen, ui.ColorClear, ui.ModifierBold)
	list.BorderStyle = ui.NewStyle(ui.ColorWhite)
	list.SelectedRowStyle = ui.NewStyle(ui.ColorBlack, ui.ColorGreen, ui.ModifierBold)
	list.TextStyle = ui.NewStyle(ui.ColorWhite)
	list.WrapText = false

	search := widgets.NewParagraph()
	search.Title = " Filter (fuzzy) "
	search.TitleStyle = ui.NewStyle(ui.ColorGreen, ui.ColorClear, ui.ModifierBold)
	search.BorderStyle = ui.NewStyle(ui.ColorWhite)

	// The column titles live outside the list so they stay put while its
	// rows scroll.
	headerBar := borderlessParagraph()
	headerBar.TextStyle = ui.NewStyle(ui.ColorCyan, ui.ColorClear, ui.ModifierBold)

	status := borderlessParagraph()
	status.TextStyle = ui.NewStyle(ui.ColorWhite)
	status.Text = "[type] filter  [↑/↓] move  [PgUp/PgDn] page  [Enter] select  [Ctrl-U] clear  [Esc/q] cancel"

	render := func() {
		w, h := ui.TerminalDimensions()
		const searchH, headerH, statusH = 3, 1, 1
		search.SetRect(0, 0, w, searchH)
		headerBar.SetRect(0, searchH, w, searchH+headerH)
		list.SetRect(0, searchH+headerH, w, h-statusH)
		status.SetRect(0, h-statusH, w, h)

		// Keep paging in step with what actually fits on screen.
		if inner := list.Inner.Dy(); inner > 0 {
			m.viewport = inner
		}

		search.Text = " " + m.query + "▏"
		// One leading space to sit under the list's left border.
		headerBar.Text = " " + m.headerRow()

		if len(m.visible()) == 0 {
			list.Rows = []string{"", "  no match — press Ctrl-U to clear the filter"}
			list.SelectedRow = 0
		} else {
			list.Rows = m.rows()
			list.SelectedRow = m.cursor
		}

		ui.Clear()
		ui.Render(search, headerBar, list, status)
	}

	render()
	for e := range ui.PollEvents() {
		switch e.Type {
		case ui.ResizeEvent:
			render()
			continue
		case ui.KeyboardEvent:
		default:
			continue
		}

		switch e.ID {
		case "<Escape>", "<C-c>", "<C-q>":
			return Item{}, ErrCancelled
		case "q":
			// q cancels only when it would not be a filter character.
			if m.query == "" {
				return Item{}, ErrCancelled
			}
			m.push('q')
		case "<Enter>":
			if sel, ok := m.selected(); ok {
				return sel, nil
			}
		case "<Up>":
			m.up()
		case "<Down>":
			m.down()
		case "<PageUp>":
			m.pageUp()
		case "<PageDown>":
			m.pageDown()
		case "<Backspace>", "<C-8>":
			m.backspace()
		case "<C-u>":
			m.clear()
		case "<Space>":
			m.push(' ')
		default:
			if utf8.RuneCountInString(e.ID) == 1 {
				m.push([]rune(e.ID)[0])
			}
		}
		render()
	}
	return Item{}, ErrCancelled
}

// borderlessParagraph returns a paragraph that actually fills a one-row rect.
// termui's Block.SetRect shrinks Inner by one cell on every side even when
// Border is false, which would leave nothing drawable; negative padding
// cancels that out.
func borderlessParagraph() *widgets.Paragraph {
	p := widgets.NewParagraph()
	p.Border = false
	p.PaddingTop = -1
	p.PaddingBottom = -1
	p.PaddingLeft = -1
	p.PaddingRight = -1
	return p
}
