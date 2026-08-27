package redash

import (
	"sort"
	"strings"
	"sync/atomic"
)

type DataSourceRegistry struct {
	byName map[string]DataSource
	all    []DataSource
}

func NewDataSourceRegistry(sources []DataSource) *DataSourceRegistry {
	r := &DataSourceRegistry{
		byName: make(map[string]DataSource, len(sources)),
		all:    sources,
	}
	for _, ds := range sources {
		r.byName[strings.ToLower(ds.Name)] = ds
	}
	return r
}

func (r *DataSourceRegistry) Lookup(name string) (DataSource, bool) {
	ds, ok := r.byName[strings.ToLower(name)]
	return ds, ok
}

func (r *DataSourceRegistry) All() []DataSource {
	return append([]DataSource(nil), r.all...)
}

func IsPostgresCompatible(dsType string) bool {
	t := strings.ToLower(dsType)
	return t == "pg" || t == "postgres" || t == "postgresql" || t == "redshift" || t == "cockroachdb" || strings.Contains(t, "postgres")
}

func IsMySQLCompatible(dsType string) bool {
	t := strings.ToLower(dsType)
	return t == "mysql" || t == "rds_mysql" || t == "aurora_mysql" || t == "mariadb" || strings.Contains(t, "mysql")
}

func FilterByType(sources []DataSource, pred func(string) bool) []DataSource {
	var filtered []DataSource
	for _, ds := range sources {
		if pred(ds.Type) {
			filtered = append(filtered, ds)
		}
	}
	return filtered
}

// SwappableRegistry is a DataSourceRegistry that the health checker can replace
// wholesale while sessions are reading it. The registry used to be built once at
// startup, so a data source added to Redash mid-session was invisible until a
// restart; now it is refreshed by every successful health probe.
//
// The pointer swap is atomic and the registry behind it is never mutated, so a
// session that is mid-lookup keeps reading a coherent snapshot.
type SwappableRegistry struct {
	current atomic.Pointer[DataSourceRegistry]
}

func NewSwappableRegistry(sources []DataSource) *SwappableRegistry {
	s := &SwappableRegistry{}
	s.Replace(sources)
	return s
}

func (s *SwappableRegistry) Replace(sources []DataSource) {
	s.current.Store(NewDataSourceRegistry(sources))
}

func (s *SwappableRegistry) Lookup(name string) (DataSource, bool) {
	return s.current.Load().Lookup(name)
}

func (s *SwappableRegistry) All() []DataSource {
	return s.current.Load().All()
}

// WireProtocol reports which wire protocol the proxy serves a data source over,
// or "" when it serves none. It mirrors the proxy's own dispatch, in the same
// order, and is the single definition shared by `datasources -json` and the
// datasources_refreshed log event, so the two can never disagree.
func WireProtocol(dsType string) string {
	switch {
	case IsPostgresCompatible(dsType):
		return "postgres"
	case IsMySQLCompatible(dsType):
		return "mysql"
	default:
		return ""
	}
}

// DataSourceView is the published shape of a data source: what the CLI prints
// with -json, and what the menu bar app reads out of the health event.
type DataSourceView struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	Wire string `json:"wire"`
}

// NewDataSourceViews sorts by name then ID, because Redash returns sources in no
// particular order and the app renders them as a menu.
func NewDataSourceViews(sources []DataSource) []DataSourceView {
	views := make([]DataSourceView, 0, len(sources))
	for _, ds := range sources {
		views = append(views, DataSourceView{
			ID:   ds.ID,
			Name: ds.Name,
			Type: ds.Type,
			Wire: WireProtocol(ds.Type),
		})
	}

	sort.Slice(views, func(i, j int) bool {
		li, lj := strings.ToLower(views[i].Name), strings.ToLower(views[j].Name)
		if li != lj {
			return li < lj
		}
		return views[i].ID < views[j].ID
	})

	return views
}
