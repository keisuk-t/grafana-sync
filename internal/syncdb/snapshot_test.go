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
		Version:    2,
		Org:        Table{Columns: []string{"id", "name"}, Rows: [][]*string{{str("9007199254740993"), str("日本語")}}},
		DataSource: Table{Columns: []string{"id", "org_id", "json_data", "name"}, Rows: [][]*string{{str("2"), str("9007199254740993"), nil, str("metrics")}}},
		DataKeys:   Table{Columns: []string{"name", "encrypted_data"}, BinaryColumns: []string{"encrypted_data"}, Rows: [][]*string{{str("key-uuid"), str("AP+A")}}},
		Secrets:    Table{Columns: []string{"id", "org_id", "namespace", "type", "value"}, Rows: [][]*string{{str("3"), str("9007199254740993"), str("metrics"), str("datasource"), str("opaque-ciphertext")}}},
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
		{"version", func(s *Snapshot) { s.Version = 3 }},
		{"legacy version", func(s *Snapshot) { s.Version = 1 }},
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
		{"null data source name", func(s *Snapshot) { s.DataSource.Rows[0][3] = nil }},
		{"empty data source name", func(s *Snapshot) { s.DataSource.Rows[0][3] = str("") }},
		{"missing key table", func(s *Snapshot) { s.DataKeys = Table{} }},
		{"missing secrets table", func(s *Snapshot) { s.Secrets = Table{} }},
		{"null key name", func(s *Snapshot) { s.DataKeys.Rows[0][0] = nil }},
		{"empty key name", func(s *Snapshot) { s.DataKeys.Rows[0][0] = str("") }},
		{"duplicate keys", func(s *Snapshot) { s.DataKeys.Rows = append(s.DataKeys.Rows, s.DataKeys.Rows[0]) }},
		{"missing binary metadata", func(s *Snapshot) { s.DataKeys.BinaryColumns = nil }},
		{"unknown binary column", func(s *Snapshot) { s.DataKeys.BinaryColumns = append(s.DataKeys.BinaryColumns, "other") }},
		{"duplicate binary column", func(s *Snapshot) { s.DataKeys.BinaryColumns = append(s.DataKeys.BinaryColumns, "encrypted_data") }},
		{"binary primary key", func(s *Snapshot) { s.DataKeys.BinaryColumns = append(s.DataKeys.BinaryColumns, "name") }},
		{"invalid base64", func(s *Snapshot) { s.DataKeys.Rows[0][1] = str("not-base64!") }},
		{"secret org", func(s *Snapshot) { s.Secrets.Rows[0][1] = str("33") }},
		{"secret namespace", func(s *Snapshot) { s.Secrets.Rows[0][2] = str("not-migrated") }},
		{"null secret namespace", func(s *Snapshot) { s.Secrets.Rows[0][2] = nil }},
		{"unrelated secret", func(s *Snapshot) { s.Secrets.Rows[0][3] = str("alerting") }},
		{"null secret type", func(s *Snapshot) { s.Secrets.Rows[0][3] = nil }},
		{"missing secret type", func(s *Snapshot) { s.Secrets.Columns[3] = "other" }},
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
	s := validSnapshot()
	for _, entry := range s.entries() {
		entry.table.Rows = [][]*string{}
	}
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
		{str("AP+A"), "BYTEA", []byte{0, 255, 128}}, {str("AP+A"), "BLOB", []byte{0, 255, 128}}, {str(""), "BLOB", []byte{}},
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

func TestBinaryCells(t *testing.T) {
	for _, value := range []any{nil, []byte{}, []byte{0, 255, 128, 0, 10, 13}, []byte("valid UTF-8 is binary too")} {
		encoded, err := encodeBinary(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{"BLOB", "BYTEA", "LONGBLOB", "VARBINARY"} {
			decoded, err := decodeCell(encoded, kind)
			if err != nil || !reflect.DeepEqual(decoded, value) {
				t.Fatalf("binary round trip: %v", err)
			}
		}
	}
	if _, err := encodeBinary("text"); err == nil {
		t.Fatal("accepted non-binary value")
	}
}

func TestInvalidImportModeDoesNotConnect(t *testing.T) {
	for _, mode := range []ImportMode{"", "append", "upsert"} {
		var db Database
		result, err := db.ImportWithMode(context.Background(), validSnapshot(), mode)
		if err == nil || result != (ImportResult{}) {
			t.Fatalf("accepted invalid mode %q", mode)
		}
	}
}

func TestInvalidSyncDoesNotConnect(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{"missing active", func(s *Snapshot) { s.DataKeys.Columns[2] = "other" }},
		{"missing org name", func(s *Snapshot) { s.Org.Columns[1] = "other" }},
		{"empty scope", func(s *Snapshot) {
			s.Org.Rows, s.DataSource.Rows, s.Secrets.Rows = [][]*string{}, [][]*string{}, [][]*string{}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := validSnapshot()
			s.DataKeys.Columns = append(s.DataKeys.Columns, "active")
			s.DataKeys.Rows[0] = append(s.DataKeys.Rows[0], str("1"))
			tc.mutate(s)
			if _, err := (&Database{}).Sync(context.Background(), s, SyncOptions{DryRun: true}); err == nil {
				t.Fatal("invalid sync accepted")
			}
		})
	}
	if _, err := (&Database{}).Sync(context.Background(), validSnapshot(), SyncOptions{}); err == nil {
		t.Fatal("missing deletion approval accepted")
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
