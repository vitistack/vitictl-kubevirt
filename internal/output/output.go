// Package output handles CLI output encoding: the -o format flag, aligned
// tables, and the machine-readable JSON/YAML renderings.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"text/tabwriter"

	"sigs.k8s.io/yaml"
)

// Format is the selected output encoding.
type Format string

const (
	FormatTable Format = ""
	FormatWide  Format = "wide"
	FormatJSON  Format = "json"
	FormatYAML  Format = "yaml"
	FormatName  Format = "name"
)

// ValidFormats lists the accepted -o values for help text. The default table
// view is the empty string and so is omitted.
var ValidFormats = []string{"wide", "json", "yaml", "name"}

// Parse validates and normalises a raw -o flag value.
func Parse(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "table":
		return FormatTable, nil
	case "wide":
		return FormatWide, nil
	case "json":
		return FormatJSON, nil
	case "yaml", "yml":
		return FormatYAML, nil
	case "name":
		return FormatName, nil
	default:
		return "", fmt.Errorf("unsupported output format %q (valid: %s)", s, strings.Join(ValidFormats, ", "))
	}
}

// IsStructured reports whether the format is machine-readable, in which case
// decorative stdout must be suppressed so the output stays parseable.
func (f Format) IsStructured() bool {
	return f == FormatJSON || f == FormatYAML
}

// WriteTable renders a tab-separated header and rows as an aligned table.
func WriteTable(w io.Writer, header string, rows []string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	if _, err := fmt.Fprintln(tw, header); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := fmt.Fprintln(tw, r); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// WriteJSON renders v as indented JSON. A nil slice is normalised to [] so
// consumers piping into jq always receive an array.
func WriteJSON(w io.Writer, v any) error {
	data, err := json.MarshalIndent(emptySliceIfNil(v), "", "  ")
	if err != nil {
		return fmt.Errorf("encoding JSON: %w", err)
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}

// WriteYAML renders v as YAML.
//
// It goes through sigs.k8s.io/yaml, which marshals to JSON first and converts.
// ROR's API types are tagged for JSON only, so a direct YAML encoder would
// emit lowercased Go field names, expose embedded structs that JSON inlines,
// and panic outright on unexported fields such as rorresources.Resource's.
func WriteYAML(w io.Writer, v any) error {
	data, err := yaml.Marshal(emptySliceIfNil(v))
	if err != nil {
		return fmt.Errorf("encoding YAML: %w", err)
	}
	_, err = w.Write(data)
	return err
}

// emptySliceIfNil replaces a nil slice with an empty one of the same type,
// turning a "null" rendering into "[]".
func emptySliceIfNil(v any) any {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Slice && rv.IsNil() {
		return reflect.MakeSlice(rv.Type(), 0, 0).Interface()
	}
	return v
}
