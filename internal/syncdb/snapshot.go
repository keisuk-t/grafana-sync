package syncdb

import (
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
	Columns []string    `json:"columns"`
	Rows    [][]*string `json:"rows"`
}

type Snapshot struct {
	Version    int   `json:"version"`
	Org        Table `json:"org"`
	DataSource Table `json:"data_source"`
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
	if s == nil || s.Version != 1 {
		return errors.New("unsupported snapshot version (expected 1)")
	}
	orgIDs, err := validateTable("org", s.Org)
	if err != nil {
		return err
	}
	if _, err := validateTable("data_source", s.DataSource); err != nil {
		return err
	}
	orgIndex := columnIndex(s.DataSource.Columns, "org_id")
	if orgIndex < 0 {
		return errors.New("data_source is missing org_id")
	}
	for i, row := range s.DataSource.Rows {
		id, err := positiveID(row[orgIndex])
		if err != nil || !orgIDs[id] {
			return fmt.Errorf("data_source row %d references an organization not in the snapshot", i+1)
		}
	}
	return nil
}

func validateTable(name string, table Table) (map[int64]bool, error) {
	seen := make(map[string]bool)
	for _, column := range table.Columns {
		if !identifier.MatchString(column) || seen[column] {
			return nil, fmt.Errorf("%s has invalid or duplicate columns", name)
		}
		seen[column] = true
	}
	idIndex := columnIndex(table.Columns, "id")
	if idIndex < 0 || table.Rows == nil {
		return nil, fmt.Errorf("%s requires id and a rows array", name)
	}
	ids := make(map[int64]bool)
	for i, row := range table.Rows {
		if len(row) != len(table.Columns) {
			return nil, fmt.Errorf("%s row %d has an incorrect cell count", name, i+1)
		}
		id, err := positiveID(row[idIndex])
		if err != nil || ids[id] {
			return nil, fmt.Errorf("%s row %d has an invalid or duplicate id", name, i+1)
		}
		ids[id] = true
	}
	return ids, nil
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
