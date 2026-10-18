package syncdb

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Non-NULL cells are strings to preserve 64-bit integers and opaque text exactly.
type Table struct {
	Columns       []string    `json:"columns"`
	BinaryColumns []string    `json:"binary_columns,omitempty"`
	Rows          [][]*string `json:"rows"`
}

type Snapshot struct {
	Version              int                   `json:"version"`
	Org                  Table                 `json:"org"`
	DataSource           Table                 `json:"data_source"`
	DataKeys             Table                 `json:"data_keys"`
	Secrets              Table                 `json:"secrets"`
	Dashboard            Table                 `json:"-"`
	Folder               Table                 `json:"-"`
	DashboardTag         Table                 `json:"-"`
	DashboardVersion     Table                 `json:"-"`
	Targets              []string              `json:"-"`
	Scope                Scope                 `json:"-"`
	DashboardPermissions []DashboardPermission `json:"-"`
}

type Scope struct {
	OrgIDs []string `json:"org_ids"`
}

type tableEntry struct {
	name    string
	key     string
	numeric bool
	table   *Table
	group   Targets
}

func (s *Snapshot) allEntries() []tableEntry {
	return []tableEntry{
		{"org", "id", true, &s.Org, TargetOrg},
		{"data_keys", "name", false, &s.DataKeys, TargetDatasource},
		{"data_source", "id", true, &s.DataSource, TargetDatasource},
		{"secrets", "id", true, &s.Secrets, TargetDatasource},
		{"folder", "id", true, &s.Folder, TargetDashboard},
		{"dashboard", "id", true, &s.Dashboard, TargetDashboard},
		{"dashboard_tag", "id", true, &s.DashboardTag, TargetDashboard},
		{"dashboard_version", "id", true, &s.DashboardVersion, TargetDashboard},
	}
}

func (s *Snapshot) entries() []tableEntry {
	var entries []tableEntry
	for _, entry := range s.allEntries() {
		if s.Selection().Has(entry.group) {
			entries = append(entries, entry)
		}
	}
	return entries
}

func (s Snapshot) MarshalJSON() ([]byte, error) {
	if s.Version != 3 {
		type legacy struct {
			Version    int   `json:"version"`
			Org        Table `json:"org"`
			DataSource Table `json:"data_source"`
			DataKeys   Table `json:"data_keys"`
			Secrets    Table `json:"secrets"`
		}
		return json.Marshal(legacy{s.Version, s.Org, s.DataSource, s.DataKeys, s.Secrets})
	}
	tables := make(map[string]Table)
	for _, entry := range s.entries() {
		tables[entry.name] = *entry.table
	}
	return json.Marshal(struct {
		Version              int                    `json:"version"`
		Targets              []string               `json:"targets"`
		Scope                Scope                  `json:"scope"`
		Tables               map[string]Table       `json:"tables"`
		DashboardPermissions *[]DashboardPermission `json:"dashboard_permissions,omitempty"`
	}{s.Version, s.Targets, s.Scope, tables, s.permissionEnvelope()})
}

func (s *Snapshot) UnmarshalJSON(data []byte) error {
	if err := uniqueJSONFields(json.NewDecoder(bytes.NewReader(data))); err != nil {
		return err
	}
	var wire struct {
		Version              int                    `json:"version"`
		Targets              []string               `json:"targets"`
		Scope                *Scope                 `json:"scope"`
		Tables               map[string]Table       `json:"tables"`
		Org                  *Table                 `json:"org"`
		DataSource           *Table                 `json:"data_source"`
		DataKeys             *Table                 `json:"data_keys"`
		Secrets              *Table                 `json:"secrets"`
		DashboardPermissions *[]DashboardPermission `json:"dashboard_permissions"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return errors.New("invalid snapshot fields")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return errors.New("invalid snapshot fields")
	}
	*s = Snapshot{Version: wire.Version}
	if wire.Version == 2 {
		for _, name := range []string{"targets", "scope", "tables", "dashboard_permissions"} {
			if _, ok := fields[name]; ok {
				return errors.New("version 2 cannot declare version 3 fields")
			}
		}
		if wire.Org != nil {
			s.Org = *wire.Org
		}
		if wire.DataSource != nil {
			s.DataSource = *wire.DataSource
		}
		if wire.DataKeys != nil {
			s.DataKeys = *wire.DataKeys
		}
		if wire.Secrets != nil {
			s.Secrets = *wire.Secrets
		}
		return nil
	}
	if wire.Version != 3 {
		return errors.New("unsupported snapshot version (expected 2 or 3)")
	}
	for _, name := range []string{"org", "data_source", "data_keys", "secrets"} {
		if _, ok := fields[name]; ok {
			return errors.New("version 3 cannot declare legacy table fields")
		}
	}
	if wire.Scope == nil || wire.Tables == nil || wire.Org != nil || wire.DataSource != nil || wire.DataKeys != nil || wire.Secrets != nil {
		return errors.New("version 3 requires targets, scope and tables only")
	}
	s.Targets, s.Scope = wire.Targets, *wire.Scope
	targets, err := ParseTargets(strings.Join(s.Targets, ","))
	if err != nil {
		return err
	}
	if targets.Has(TargetDashboard) != (wire.DashboardPermissions != nil) {
		return errors.New("dashboard target requires an explicit dashboard_permissions array")
	}
	if _, ok := fields["dashboard_permissions"]; ok && !targets.Has(TargetDashboard) {
		return errors.New("unselected dashboard cannot declare dashboard_permissions")
	}
	if wire.DashboardPermissions != nil {
		s.DashboardPermissions = *wire.DashboardPermissions
	}
	for _, entry := range s.allEntries() {
		table, ok := wire.Tables[entry.name]
		if targets.Has(entry.group) != ok {
			return errors.New("snapshot tables do not match declared targets")
		}
		if ok {
			*entry.table = table
			delete(wire.Tables, entry.name)
		}
	}
	if len(wire.Tables) != 0 {
		return errors.New("snapshot contains unknown tables")
	}
	return nil
}

func uniqueJSONFields(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return errors.New("invalid snapshot JSON")
	}
	if delimiter, ok := token.(json.Delim); ok {
		seen := make(map[string]bool)
		for dec.More() {
			if delimiter == '{' {
				key, err := dec.Token()
				if err != nil {
					return errors.New("invalid snapshot JSON")
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("duplicate or invalid JSON field")
				}
				seen[name] = true
			}
			if err := uniqueJSONFields(dec); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return errors.New("invalid snapshot JSON")
		}
	}
	return nil
}

var identifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

func Decode(r io.Reader) (*Snapshot, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var s Snapshot
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("invalid snapshot JSON: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, errors.New("unexpected content after snapshot")
	}
	return &s, s.Validate()
}

func (s *Snapshot) Validate() error {
	if s == nil || (s.Version != 2 && s.Version != 3) {
		return errors.New("unsupported snapshot version (expected 2 or 3; re-export to include encryption tables)")
	}
	if err := s.Selection().Validate(); err != nil {
		return err
	}
	if s.Version == 3 && len(s.Targets) != len(s.Selection().Names()) {
		return errors.New("targets must list individual resource names")
	}
	for _, entry := range s.entries() {
		if err := validateTable(entry); err != nil {
			return err
		}
	}
	orgIDs := s.scopeIDs()
	if s.Version == 3 {
		if s.Scope.OrgIDs == nil {
			return errors.New("scope requires an org_ids array")
		}
		seen := make(map[int64]bool)
		for _, value := range s.Scope.OrgIDs {
			id, err := positiveID(&value)
			if err != nil || seen[id] {
				return errors.New("scope contains invalid or duplicate organization ids")
			}
			seen[id] = true
		}
	}
	orgIDIndex := columnIndex(s.Org.Columns, "id")
	listed := make(map[int64]bool)
	for _, row := range s.Org.Rows {
		id, _ := positiveID(row[orgIDIndex])
		listed[id] = true
		if !orgIDs[id] {
			return errors.New("organization is outside snapshot scope")
		}
	}
	if s.Selection().Has(TargetOrg) && len(listed) != len(orgIDs) {
		return errors.New("organization rows differ from snapshot scope")
	}
	if s.Selection().Has(TargetDashboard) {
		if err := s.validateDashboards(orgIDs); err != nil {
			return err
		}
	}
	if !s.Selection().Has(TargetDatasource) {
		return nil
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
