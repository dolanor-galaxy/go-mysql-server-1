package memory

import (
	"github.com/src-d/go-mysql-server/sql"
)

type AscendIndexLookup struct {
	id  string
	Gte []interface{}
	Lt  []interface{}
}

func (l *AscendIndexLookup) ID() string {
	return l.id
}

func (l *AscendIndexLookup) GetUnions() []MergeableLookup {
	return nil
}

func (l *AscendIndexLookup) GetIntersections() []MergeableLookup {
	return nil
}

func (AscendIndexLookup) Values(sql.Partition) (sql.IndexValueIter, error) {
	return nil, nil
}

func (l *AscendIndexLookup) Indexes() []string {
	return []string{l.id}
}

func (l *AscendIndexLookup) IsMergeable(sql.IndexLookup) bool {
	return true
}

func (l *AscendIndexLookup) Union(lookups ...sql.IndexLookup) sql.IndexLookup {
	var unions []MergeableLookup
	unions = append(unions, l)
	for _, idx := range lookups {
		unions = append(unions, idx.(MergeableLookup))
	}

	return &MergedIndexLookup{
		Unions: unions,
	}
}

func (AscendIndexLookup) Difference(...sql.IndexLookup) sql.IndexLookup {
	panic("ascendIndexLookup.Difference is not implemented")
}

func (AscendIndexLookup) Intersection(...sql.IndexLookup) sql.IndexLookup {
	panic("ascendIndexLookup.Intersection is not implemented")
}
