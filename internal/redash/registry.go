package redash

import (
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

type DataSourceRegistry struct {
	// byExact resolves a name that matches a data source's name character for
	// character; byLower is the case-insensitive fallback. Redash returns names
	// in no particular order, so a case-insensitive map alone was last-wins and
	// resolved "Prod" vs "prod" nondeterministically from one refresh to the next.
	byExact map[string]DataSource
	byLower map[string]DataSource
	all     []DataSource
}

// loggedCollisions remembers which case-insensitive collisions have already been
// warned about, so a persistent misconfiguration is reported once rather than on
// every ten-second registry refresh.
var loggedCollisions sync.Map

func NewDataSourceRegistry(sources []DataSource) *DataSourceRegistry {
	r := &DataSourceRegistry{
		byExact: make(map[string]DataSource, len(sources)),
		byLower: make(map[string]DataSource, len(sources)),
		all:     sources,
	}
	for _, ds := range sources {
		r.byExact[ds.Name] = ds

		lower := strings.ToLower(ds.Name)
		existing, clash := r.byLower[lower]
		if !clash {
			r.byLower[lower] = ds
			continue
		}
		// Two names differ only in case. Pick deterministically (lowest id) so the
		// case-insensitive fallback is stable across refreshes, and warn once.
		if ds.ID < existing.ID {
			r.byLower[lower] = ds
		}
		if _, seen := loggedCollisions.LoadOrStore(lower, struct{}{}); !seen {
			slog.Warn("data source names collide case-insensitively; resolving the ambiguous name to the lowest id",
				"name", lower, "ids", []int{existing.ID, ds.ID})
		}
	}
	return r
}

// Lookup prefers an exact-case match, so a data source named exactly as the
// client asked always wins; only when there is no exact match does it fall back
// to the case-insensitive entry.
func (r *DataSourceRegistry) Lookup(name string) (DataSource, bool) {
	if ds, ok := r.byExact[name]; ok {
		return ds, true
	}
	ds, ok := r.byLower[strings.ToLower(name)]
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
