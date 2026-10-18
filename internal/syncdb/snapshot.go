package syncdb

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
)

// Non-NULL cells are strings to preserve 64-bit integers and opaque text exactly.
type Table struct {
	Columns       []string    `json:"columns"`
	BinaryColumns []string    `json:"binary_columns,omitempty"`
	Rows          [][]*string `json:"rows"`
}

type Snapshot struct {
	Version    int   `json:"version"`
	Org        Table `json:"org"`
	DataSource Table `json:"data_source"`
	DataKeys   Table `json:"data_keys"`
	Secrets    Table `json:"secrets"`
}

type tableEntry struct {
	name    string
	key     string
	numeric bool
	table   *Table
}

func (s *Snapshot) entries() []tableEntry {
	return []tableEntry{
		{"org", "id", true, &s.Org},
		{"data_keys", "name", false, &s.DataKeys},
		{"data_source", "id", true, &s.DataSource},
		{"secrets", "id", true, &s.Secrets},
	}
}

var identifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

func Decode(r io.Reader) (*Snapshot, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var s Snapshot
	if err := dec.Decode(&s); err != nil {
		return nil, errors.New("invalid snapshot JSON")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, errors.New("unexpected content after snapshot")
	}
	return &s, s.Validate()
}

func (s *Snapshot) Validate() error {
	if s == nil || s.Version != 2 {
		return errors.New("unsupported snapshot version (expected 2; re-export to include encryption tables)")
	}
	for _, entry := range s.entries() {
		if err := validateTable(entry); err != nil {
			return err
		}
	}
	orgIDs := make(map[int64]bool)
	orgIDIndex := columnIndex(s.Org.Columns, "id")
	for _, row := range s.Org.Rows {
		id, _ := positiveID(row[orgIDIndex])
		orgIDs[id] = true
	}
	orgIndex := columnIndex(s.DataSource.Columns, "org_id")
	nameIndex := columnIndex(s.DataSource.Columns, "name")
	if orgIndex < 0 || nameIndex < 0 {
		return errors.New("data_source requires org_id and name")
	}
	type datasourceKey struct {
		orgID int64
		name  string
	}
	datasources := make(map[datasourceKey]bool)
	for i, row := range s.DataSource.Rows {
		id, err := positiveID(row[orgIndex])
		if err != nil || !orgIDs[id] {
			return fmt.Errorf("data_source row %d references an organization not in the snapshot", i+1)
		}
		if row[nameIndex] == nil || *row[nameIndex] == "" {
			return fmt.Errorf("data_source row %d has an invalid name", i+1)
		}
		datasources[datasourceKey{id, *row[nameIndex]}] = true
	}
	if columnIndex(s.DataKeys.BinaryColumns, "encrypted_data") < 0 {
		return errors.New("data_keys requires binary encrypted_data")
	}
	for _, required := range []string{"org_id", "namespace", "type", "value"} {
		if columnIndex(s.Secrets.Columns, required) < 0 {
			return fmt.Errorf("secrets is missing %s", required)
		}
	}
	secretOrgIndex := columnIndex(s.Secrets.Columns, "org_id")
	secretNamespaceIndex := columnIndex(s.Secrets.Columns, "namespace")
	secretTypeIndex := columnIndex(s.Secrets.Columns, "type")
	for i, row := range s.Secrets.Rows {
		orgID, err := positiveID(row[secretOrgIndex])
		namespace := row[secretNamespaceIndex]
		kind := row[secretTypeIndex]
		if err != nil || namespace == nil || kind == nil || *kind != "datasource" || !datasources[datasourceKey{orgID, *namespace}] {
			return fmt.Errorf("secrets row %d does not belong to a migrated data source", i+1)
		}
	}
	return nil
}

func validateTable(entry tableEntry) error {
	name, table := entry.name, entry.table
	seen := make(map[string]bool)
	for _, column := range table.Columns {
		if !identifier.MatchString(column) || seen[column] {
			return fmt.Errorf("%s has invalid or duplicate columns", name)
		}
		seen[column] = true
	}
	keyIndex := columnIndex(table.Columns, entry.key)
	if keyIndex < 0 || table.Rows == nil {
		return fmt.Errorf("%s requires %s and a rows array", name, entry.key)
	}
	binary := make(map[string]bool)
	for _, column := range table.BinaryColumns {
		if !seen[column] || binary[column] || column == entry.key {
			return fmt.Errorf("%s has invalid binary columns", name)
		}
		binary[column] = true
	}
	keys := make(map[string]bool)
	for i, row := range table.Rows {
		if len(row) != len(table.Columns) {
			return fmt.Errorf("%s row %d has an incorrect cell count", name, i+1)
		}
		key := row[keyIndex]
		if key == nil || *key == "" {
			return fmt.Errorf("%s row %d has an invalid key", name, i+1)
		}
		canonical := *key
		if entry.numeric {
			id, err := positiveID(key)
			if err != nil {
				return fmt.Errorf("%s row %d has an invalid id", name, i+1)
			}
			canonical = strconv.FormatInt(id, 10)
		}
		if keys[canonical] {
			return fmt.Errorf("%s row %d has a duplicate key", name, i+1)
		}
		keys[canonical] = true
		for j, value := range row {
			if binary[table.Columns[j]] && value != nil {
				if _, err := base64.StdEncoding.Strict().DecodeString(*value); err != nil {
					return fmt.Errorf("%s row %d has invalid base64", name, i+1)
				}
			}
		}
	}
	return nil
}

func positiveID(value *string) (int64, error) {
	if value != nil {
		id, err := strconv.ParseInt(*value, 10, 64)
		if err == nil && id > 0 && id < math.MaxInt64 {
			return id, nil
		}
	}
	return 0, errors.New("invalid id")
}

func columnIndex(columns []string, name string) int {
	for i, column := range columns {
		if column == name {
			return i
		}
	}
	return -1
}
