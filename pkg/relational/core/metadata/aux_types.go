package metadata

import (
	"fdb.dev/pkg/relational/api"
)

// Auxiliary-type registry + type resolution — the port of Java's
// RecordLayerSchemaTemplate.Builder auxiliary machinery
// (RecordLayerSchemaTemplate.java:409, :552-556, :592-603, :642-751): a
// `CREATE TYPE AS STRUCT` registers a named type that table columns may
// reference — by name, forwards or backwards — and Build() resolves every
// UnresolvedType placeholder through a dependency-ordered pass that rejects
// cycles.

// AddAuxiliaryType registers a named auxiliary type (a struct declared with
// CREATE TYPE AS STRUCT). Mirrors Builder.addAuxiliaryType: name collisions
// against tables and other auxiliary types are rejected
// (verifyNameIsNotUsed).
func (b *Builder) AddAuxiliaryType(t api.Named) *Builder {
	if err := b.verifyNameIsNotUsed(t.Name()); err != nil {
		b.errs = append(b.errs, err)
		return b
	}
	b.auxTypes = append(b.auxTypes, t)
	return b
}

// verifyNameIsNotUsed mirrors Java's verifyNameIsNotUsed (tables, auxiliary
// types; Go has no routines/views to collide with yet).
func (b *Builder) verifyNameIsNotUsed(name string) error {
	for _, tbl := range b.tables {
		if tbl.name == name {
			return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"table with name '%s' already exists", name)
		}
	}
	for _, t := range b.auxTypes {
		if t.Name() == name {
			return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"type with name '%s' already exists", name)
		}
	}
	return nil
}

// FindType mirrors Builder.findType: tables FIRST (a table's name is itself
// usable as a column type — its row struct), then auxiliary types.
func (b *Builder) FindType(name string) (api.DataType, bool) {
	for _, tbl := range b.tables {
		if tbl.name == name {
			return b.tableStructType(tbl), true
		}
	}
	for _, t := range b.auxTypes {
		if t.Name() == name {
			if dt, ok := t.(api.DataType); ok {
				return dt, true
			}
		}
	}
	return nil, false
}

// tableStructType is the table's row type as a DataType — Java's
// RecordLayerTable.getDatatype() (calculateDataType: StructType.from(name,
// columns, nullable=true)).
func (b *Builder) tableStructType(tbl tableSpec) *api.StructType {
	fields := make([]api.StructField, len(tbl.columns))
	for i, col := range tbl.columns {
		fields[i] = api.NewStructField(col.name, col.dt, i)
	}
	return api.NewStructType(tbl.name, fields, true)
}

// needsTypeResolution reports whether any table column or auxiliary type
// still carries an UnresolvedType — Java's needsResolution scan in build().
func (b *Builder) needsTypeResolution() bool {
	for _, tbl := range b.tables {
		for _, col := range tbl.columns {
			if !col.dt.IsResolved() {
				return true
			}
		}
	}
	for _, t := range b.auxTypes {
		if dt, ok := t.(api.DataType); ok && !dt.IsResolved() {
			return true
		}
	}
	return false
}

// resolveTypes ports Builder.resolveTypes: collect the named types (tables +
// auxiliary), build the dependency graph, topologically sort it — a cycle is
// "Invalid cyclic dependency in the schema definition" — then resolve each
// type in dependency order and rewrite table columns and auxiliary types
// through the resolution map.
func (b *Builder) resolveTypes() error {
	namedTypes := make(map[string]api.DataType, len(b.tables)+len(b.auxTypes))
	order := make([]string, 0, len(b.tables)+len(b.auxTypes))
	for _, tbl := range b.tables {
		namedTypes[tbl.name] = b.tableStructType(tbl)
		order = append(order, tbl.name)
	}
	for _, t := range b.auxTypes {
		if dt, ok := t.(api.DataType); ok {
			namedTypes[t.Name()] = dt
			order = append(order, t.Name())
		}
	}

	// Dependency edges: node -> the named types it references.
	deps := make(map[string][]string, len(namedTypes))
	for name, dt := range namedTypes {
		d, err := typeDependencies(dt, namedTypes)
		if err != nil {
			return err
		}
		deps[name] = d
	}

	// Kahn's algorithm over the name graph (Java:
	// TopologicalSort.anyTopologicalOrderPermutation; empty result = cycle).
	// Iterating `order` keeps the walk deterministic.
	indegree := make(map[string]int, len(namedTypes))
	dependents := make(map[string][]string, len(namedTypes))
	for _, name := range order {
		for _, dep := range deps[name] {
			if dep == name {
				// Self-reference is the smallest cycle.
				return api.NewError(api.ErrCodeInvalidSchemaTemplate,
					"Invalid cyclic dependency in the schema definition")
			}
			indegree[name]++
			dependents[dep] = append(dependents[dep], name)
		}
	}
	queue := make([]string, 0, len(order))
	for _, name := range order {
		if indegree[name] == 0 {
			queue = append(queue, name)
		}
	}
	resolved := make(map[string]api.Named, len(namedTypes))
	processed := 0
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		processed++
		dt := namedTypes[name]
		if !dt.IsResolved() {
			dt = dt.Resolve(resolved)
		}
		if named, ok := dt.(api.Named); ok {
			resolved[name] = named
		}
		for _, dep := range dependents[name] {
			indegree[dep]--
			if indegree[dep] == 0 {
				queue = append(queue, dep)
			}
		}
	}
	if processed != len(order) {
		return api.NewError(api.ErrCodeInvalidSchemaTemplate,
			"Invalid cyclic dependency in the schema definition")
	}

	// Rewrite table columns through the resolution map (Java rebuilds each
	// unresolved table from its resolved StructType).
	for ti := range b.tables {
		for ci := range b.tables[ti].columns {
			col := &b.tables[ti].columns[ci]
			if !col.dt.IsResolved() {
				col.dt = col.dt.Resolve(resolved)
			}
		}
	}
	for i, t := range b.auxTypes {
		if dt, ok := t.(api.DataType); ok && !dt.IsResolved() {
			if named, ok2 := dt.Resolve(resolved).(api.Named); ok2 {
				b.auxTypes[i] = named
			}
		}
	}
	return nil
}

// typeDependencies ports Builder.getDependencies: the named types a type
// references — struct fields that are Named types (structs, unresolved
// forward references) or arrays of Named element types; an UnresolvedType
// depends on its target. Unknown names die with Java's
// "could not find type '%s'" (UNKNOWN_TYPE).
func typeDependencies(dt api.DataType, types map[string]api.DataType) ([]string, error) {
	switch t := dt.(type) {
	case *api.ArrayType:
		return typeDependencies(t.ElementType(), types)
	case *api.StructType:
		var out []string
		seen := map[string]bool{}
		add := func(name string) error {
			if _, ok := types[name]; !ok {
				return api.NewErrorf(api.ErrCodeUnknownType,
					"could not find type '%s'", name)
			}
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
			return nil
		}
		for _, f := range t.Fields() {
			ft := f.Type()
			if at, isArr := ft.(*api.ArrayType); isArr {
				ft = at.ElementType()
			}
			if named, isNamed := ft.(api.Named); isNamed {
				if err := add(named.Name()); err != nil {
					return nil, err
				}
			}
		}
		return out, nil
	case *api.UnresolvedType:
		if _, ok := types[t.Name()]; !ok {
			return nil, api.NewErrorf(api.ErrCodeUnknownType,
				"could not find type '%s'", t.Name())
		}
		return []string{t.Name()}, nil
	default:
		return nil, nil
	}
}
