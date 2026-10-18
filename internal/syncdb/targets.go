package syncdb

import (
	"errors"
	"strings"
)

type Targets uint8

const (
	TargetOrg Targets = 1 << iota
	TargetDatasource
	TargetDashboard
	DefaultTargets = TargetOrg | TargetDatasource
	AllTargets     = DefaultTargets | TargetDashboard
)

func ParseTargets(value string) (Targets, error) {
	var targets Targets
	for _, name := range strings.Split(value, ",") {
		var target Targets
		switch name {
		case "org":
			target = TargetOrg
		case "datasource":
			target = TargetDatasource
		case "dashboard":
			target = TargetDashboard
		default:
			return 0, errors.New("targets must be a nonempty combination of org,datasource,dashboard")
		}
		if targets&target != 0 {
			return 0, errors.New("duplicate target")
		}
		targets |= target
	}
	return targets, nil
}

func (t Targets) Validate() error {
	if t == 0 || t&^AllTargets != 0 {
		return errors.New("targets must be a nonempty combination of org,datasource,dashboard")
	}
	return nil
}

func (t Targets) Has(target Targets) bool { return t&target != 0 }

func (t Targets) Names() []string {
	names := []string{}
	for _, entry := range []struct {
		target Targets
		name   string
	}{
		{TargetOrg, "org"}, {TargetDatasource, "datasource"}, {TargetDashboard, "dashboard"},
	} {
		if t.Has(entry.target) {
			names = append(names, entry.name)
		}
	}
	return names
}

func (t Targets) String() string { return strings.Join(t.Names(), ",") }

func (s *Snapshot) Selection() Targets {
	if s.Version == 2 {
		return DefaultTargets
	}
	targets, _ := ParseTargets(strings.Join(s.Targets, ","))
	return targets
}

func (s *Snapshot) ScopeIDs() []string {
	if s.Version >= 3 {
		return s.Scope.OrgIDs
	}
	ids := []string{}
	index := columnIndex(s.Org.Columns, "id")
	if index >= 0 {
		for _, row := range s.Org.Rows {
			ids = append(ids, *row[index])
		}
	}
	return ids
}

func (s *Snapshot) scopeIDs() map[int64]bool {
	ids := make(map[int64]bool)
	for _, text := range s.ScopeIDs() {
		id, _ := positiveID(&text)
		ids[id] = true
	}
	return ids
}

func (s *Snapshot) Select(targets Targets) (*Snapshot, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if err := targets.Validate(); err != nil {
		return nil, err
	}
	if targets&^s.Selection() != 0 {
		return nil, errors.New("requested targets are absent from snapshot; use --targets matching its targets")
	}
	selected := *s
	selected.Version, selected.Targets = 3, targets.Names()
	if s.Version >= 4 {
		selected.Version = s.Version
	}
	selected.Scope = Scope{OrgIDs: append([]string{}, s.ScopeIDs()...)}
	if !targets.Has(TargetDashboard) {
		selected.LibraryPanelsExcluded = false
		selected.DashboardPermissions = nil
		selected.RBAC = nil
	}
	for _, entry := range selected.allEntries() {
		if !targets.Has(entry.group) {
			*entry.table = Table{}
		}
	}
	return &selected, selected.Validate()
}
