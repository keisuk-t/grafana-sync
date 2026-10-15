package syncdb

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func str(s string) *string { return &s }

func validSnapshot() *Snapshot {
	return &Snapshot{
		Version:    1,
		Org:        Table{Columns: []string{"id", "name"}, Rows: [][]*string{{str("9007199254740993"), str("日本語")}}},
		DataSource: Table{Columns: []string{"id", "org_id", "json_data"}, Rows: [][]*string{{str("2"), str("9007199254740993"), nil}}},
	}
}

func TestDecode(t *testing.T) {
	s := validSnapshot()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(strings.NewReader(string(data)))
	if err != nil || !reflect.DeepEqual(s, decoded) {
		t.Fatalf("round trip: %v", err)
	}
	for _, suffix := range []string{` {}`, ` invalid`} {
		if _, err := Decode(strings.NewReader(string(data) + suffix)); err == nil {
			t.Fatal("accepted trailing data")
		}
	}
	for _, input := range []string{`{}`, `null`, `{"version":1,"dashboard":{}}`, `{"version":1,"org":{"columns":["id"],"rows":null}}`, `not json`} {
		if _, err := Decode(strings.NewReader(input)); err == nil {
			t.Fatalf("accepted invalid snapshot %s", input)
		}
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{"version", func(s *Snapshot) { s.Version = 2 }},
		{"missing id", func(s *Snapshot) { s.Org.Columns[0] = "other" }},
		{"injection", func(s *Snapshot) { s.Org.Columns[1] = `name); DROP TABLE dashboard;--` }},
		{"duplicate columns", func(s *Snapshot) { s.Org.Columns[1] = "id" }},
		{"width", func(s *Snapshot) { s.Org.Rows[0] = nil }},
		{"null id", func(s *Snapshot) { s.Org.Rows[0][0] = nil }},
		{"zero id", func(s *Snapshot) { s.Org.Rows[0][0] = str("0") }},
		{"sequence overflow", func(s *Snapshot) { s.Org.Rows[0][0] = str("9223372036854775807") }},
		{"duplicate ids", func(s *Snapshot) { s.Org.Rows = append(s.Org.Rows, s.Org.Rows[0]) }},
		{"missing org", func(s *Snapshot) { s.DataSource.Rows[0][1] = str("22") }},
		{"null org", func(s *Snapshot) { s.DataSource.Rows[0][1] = nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := validSnapshot()
			tc.mutate(s)
			if s.Validate() == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	s := &Snapshot{Version: 1, Org: Table{Columns: []string{"id"}, Rows: [][]*string{}}, DataSource: Table{Columns: []string{"id", "org_id"}, Rows: [][]*string{}}}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCells(t *testing.T) {
	stamp := time.Date(2025, 1, 2, 3, 4, 5, 123456000, time.UTC)
	for _, value := range []any{nil, "", "日本語", []byte("opaque cipher"), int64(9007199254740993), true, false, stamp} {
		if _, err := encodeCell(value); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []any{float64(1.5), []byte{0xff}, time.Time{}} {
		if _, err := encodeCell(value); err == nil {
			t.Fatal("expected unsupported value error")
		}
	}
	tests := []struct {
		value *string
		kind  string
		want  any
	}{
		{nil, "TEXT", nil}, {str(""), "VARCHAR", ""}, {str("9007199254740993"), "INT8", int64(9007199254740993)},
		{str("1"), "BOOL", true}, {str("0"), "BOOLEAN", false}, {str(stamp.Format(time.RFC3339Nano)), "DATETIME", stamp},
	}
	for _, tc := range tests {
		got, err := decodeCell(tc.value, tc.kind)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("decode %s: %v %v", tc.kind, got, err)
		}
	}
	for _, tc := range []struct{ value, kind string }{{"no", "INT8"}, {"bad", "DATETIME"}, {"2", "BOOL"}, {"x", "BYTEA"}} {
		if _, err := decodeCell(str(tc.value), tc.kind); err == nil {
			t.Fatal("accepted invalid cell")
		}
	}
}

func TestOpenDoesNotExposeDSN(t *testing.T) {
	for _, engine := range []string{"mysql", "postgres", "sqlite"} {
		_, err := Open(context.Background(), engine, "not-a-dsn/secret-password%")
		if err == nil || strings.Contains(err.Error(), "secret-password") {
			t.Fatalf("unsafe error: %v", err)
		}
	}
}
