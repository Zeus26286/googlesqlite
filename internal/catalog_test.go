package internal

import (
	"slices"
	"testing"

	googlesql "github.com/goccy/go-googlesql"
	"github.com/google/go-cmp/cmp"
)

func TestNormalizeTablePath(t *testing.T) {
	c := &Catalog{}

	tests := []struct {
		name     string
		input    []string
		expected []string
	}{
		{
			name:     "already normalized path",
			input:    []string{"project", "dataset", "table"},
			expected: []string{"project", "dataset", "table"},
		},
		{
			name:     "single element with dots",
			input:    []string{"project.dataset.table"},
			expected: []string{"project", "dataset", "table"},
		},
		{
			name:     "mixed normalized and dot-separated",
			input:    []string{"project", "dataset.table"},
			expected: []string{"project", "dataset", "table"},
		},
		{
			name:     "two element path",
			input:    []string{"dataset", "table"},
			expected: []string{"dataset", "table"},
		},
		{
			name:     "single element no dots",
			input:    []string{"table"},
			expected: []string{"table"},
		},
		{
			name:     "empty path",
			input:    []string{},
			expected: []string{},
		},
		{
			name:     "project with hyphen",
			input:    []string{"my-project.dataset.table"},
			expected: []string{"my-project", "dataset", "table"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := c.normalizeTablePath(tt.input)
			if diff := cmp.Diff(tt.expected, result); diff != "" {
				t.Errorf("normalizeTablePath(%v) mismatch (-want +got):\n%s", tt.input, diff)
			}
		})
	}
}

// TestSubCatalogsHoldNoBuiltins guards against copying the builtin
// function library into the project / dataset sub-catalogs a table
// path creates. Each copy costs about 1 MiB of wasm heap, and a
// consumer that creates a dataset per query job (such as a BigQuery
// emulator writing job results into anonymous datasets) grows the
// heap with every job until the 32-bit wasm address space runs out.
func TestSubCatalogsHoldNoBuiltins(t *testing.T) {
	c := NewCatalog(nil)
	spec := &TableSpec{
		NamePath: []string{"proj", "ds", "tbl"},
		Columns: []*ColumnSpec{
			{Name: "id", Type: &Type{Kind: int(googlesql.TypeKindTypeInt64)}},
		},
	}
	if err := c.addTableSpec(spec); err != nil {
		t.Fatalf("addTableSpec: %v", err)
	}
	var subs int
	for _, children := range c.subCatalogs {
		for name, sub := range children {
			subs++
			funcs, err := sub.FunctionNames()
			if err != nil {
				t.Fatalf("sub-catalog %q: FunctionNames: %v", name, err)
			}
			if len(funcs) != 0 {
				t.Errorf("sub-catalog %q holds %d functions, want 0 (first: %q)", name, len(funcs), funcs[0])
			}
		}
	}
	if subs == 0 {
		t.Fatal("addTableSpec created no sub-catalogs")
	}
	// The table must still resolve through the sub-catalogs.
	names, err := c.getOrCreateSubCatalog(c.getOrCreateSubCatalog(c.catalog, "proj"), "ds").TableNames()
	if err != nil {
		t.Fatalf("proj.ds sub-catalog: TableNames: %v", err)
	}
	if !slices.Contains(names, "tbl") {
		t.Fatalf("proj.ds sub-catalog tables = %v, want them to include tbl", names)
	}
}
