package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		in      string
		want    Format
		wantErr bool
	}{
		{"", FormatTable, false},
		{"table", FormatTable, false},
		{"wide", FormatWide, false},
		{"JSON", FormatJSON, false},
		{"yaml", FormatYAML, false},
		{"yml", FormatYAML, false},
		{"name", FormatName, false},
		{"xml", "", true},
	}
	for _, tt := range tests {
		got, err := Parse(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("Parse(%q) = nil error, want one", tt.in)
			} else if !strings.Contains(err.Error(), "wide") {
				t.Errorf("Parse(%q) error %q should list the valid formats", tt.in, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q) error = %v", tt.in, err)
		}
		if got != tt.want {
			t.Errorf("Parse(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestIsStructured(t *testing.T) {
	for _, f := range []Format{FormatJSON, FormatYAML} {
		if !f.IsStructured() {
			t.Errorf("%q.IsStructured() = false, want true", f)
		}
	}
	for _, f := range []Format{FormatTable, FormatWide, FormatName} {
		if f.IsStructured() {
			t.Errorf("%q.IsStructured() = true, want false", f)
		}
	}
}

// Columns must line up even when cell widths differ wildly, otherwise the
// table is unreadable for real cluster names.
func TestWriteTableAlignsColumns(t *testing.T) {
	var buf bytes.Buffer
	err := WriteTable(&buf, "NAME\tENV", []string{
		"a\tprod",
		"a-very-long-cluster-name\tdev",
	})
	if err != nil {
		t.Fatalf("WriteTable() error = %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3 (header + 2 rows): %q", len(lines), lines)
	}
	envCol := strings.Index(lines[0], "ENV")
	if envCol <= 0 {
		t.Fatalf("no ENV column in header %q", lines[0])
	}
	for _, l := range lines[1:] {
		if got := strings.Index(l, strings.TrimSpace(l[envCol:])); got != envCol {
			t.Errorf("row %q does not start its second column at %d", l, envCol)
		}
	}
}

func TestWriteJSONEmitsArrayForSlices(t *testing.T) {
	var buf bytes.Buffer
	type row struct {
		Name string `json:"name"`
	}
	if err := WriteJSON(&buf, []row{{Name: "a"}, {Name: "b"}}); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}

	var got []row
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not a JSON array: %v\n%s", err, buf.String())
	}
	if len(got) != 2 || got[0].Name != "a" {
		t.Errorf("round-trip = %+v, want two rows starting with a", got)
	}
}

// An empty result must still be valid JSON an array-consumer can parse,
// not the bare "null" that a nil slice marshals to.
func TestWriteJSONEmitsEmptyArrayNotNull(t *testing.T) {
	var buf bytes.Buffer
	var none []struct{}
	if err := WriteJSON(&buf, none); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "[]" {
		t.Errorf("WriteJSON(nil slice) = %q, want %q", got, "[]")
	}
}

// ROR's API types are tagged for JSON only. YAML output must follow those
// same tags — and honour inlining and omitempty — so -o yaml and -o json
// describe the same document rather than leaking Go field names.
func TestWriteYAMLFollowsJSONTags(t *testing.T) {
	type meta struct {
		Name string `json:"name"`
	}
	type envelope struct {
		Kind     string `json:"kind"`
		Metadata meta   `json:"metadata"`
	}
	type row struct {
		envelope `json:",inline"`
		Ignored  string `json:"ignored,omitempty"`
	}

	var buf bytes.Buffer
	if err := WriteYAML(&buf, []row{{envelope: envelope{Kind: "KubernetesCluster", Metadata: meta{Name: "a"}}}}); err != nil {
		t.Fatalf("WriteYAML() error = %v", err)
	}
	got := buf.String()

	for _, want := range []string{"kind: KubernetesCluster", "name: a"} {
		if !strings.Contains(got, want) {
			t.Errorf("WriteYAML() = %q, want it to contain %q", got, want)
		}
	}
	// The embedded struct is inlined in JSON; it must not surface as a key.
	if strings.Contains(got, "envelope:") {
		t.Errorf("WriteYAML() = %q, leaked the embedded Go struct name", got)
	}
	if strings.Contains(got, "ignored:") {
		t.Errorf("WriteYAML() = %q, emitted an omitempty field that is empty", got)
	}
}
