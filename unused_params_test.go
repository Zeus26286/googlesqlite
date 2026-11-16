package googlesqlite_test

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
)

// TestUnusedNamedParamIgnored checks that named parameters a statement
// never references are ignored, as BigQuery does, instead of making
// SQLite reject the statement for receiving unexpected arguments.
func TestUnusedNamedParamIgnored(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("googlesqlite", ":memory:?_test=unused_params_named")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	var v int64
	if err := db.QueryRowContext(ctx, "SELECT 1", sql.Named("a", int64(1))).Scan(&v); err != nil {
		t.Fatalf("one unused named param: %v", err)
	}
	if v != 1 {
		t.Fatalf("one unused named param: got %d, want 1", v)
	}
	if err := db.QueryRowContext(ctx, "SELECT 1", sql.Named("a", int64(1)), sql.Named("b", "x")).Scan(&v); err != nil {
		t.Fatalf("two unused named params: %v", err)
	}
	if v != 1 {
		t.Fatalf("two unused named params: got %d, want 1", v)
	}
}

// TestUnusedPositionalParamIgnored is the positional counterpart of
// TestUnusedNamedParamIgnored.
func TestUnusedPositionalParamIgnored(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("googlesqlite", ":memory:?_test=unused_params_positional")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	var v int64
	if err := db.QueryRowContext(ctx, "SELECT 1", int64(5)).Scan(&v); err != nil {
		t.Fatalf("one unused positional param: %v", err)
	}
	if v != 1 {
		t.Fatalf("one unused positional param: got %d, want 1", v)
	}
	if err := db.QueryRowContext(ctx, "SELECT 1", int64(5), "x").Scan(&v); err != nil {
		t.Fatalf("two unused positional params: %v", err)
	}
	if v != 1 {
		t.Fatalf("two unused positional params: got %d, want 1", v)
	}
}

// TestPartiallyUsedNamedParams guards the existing behavior when only
// some of the supplied parameters are referenced: the used ones must
// still bind correctly while the extra ones are ignored.
func TestPartiallyUsedNamedParams(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("googlesqlite", ":memory:?_test=unused_params_partial")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	var v int64
	if err := db.QueryRowContext(ctx, "SELECT @a", sql.Named("a", int64(7)), sql.Named("b", "x")).Scan(&v); err != nil {
		t.Fatalf("partially used named params: %v", err)
	}
	if v != 7 {
		t.Fatalf("partially used named params: got %d, want 7", v)
	}
	if err := db.QueryRowContext(ctx, "SELECT ?", int64(7), int64(8)).Scan(&v); err != nil {
		t.Fatalf("partially used positional params: %v", err)
	}
	if v != 7 {
		t.Fatalf("partially used positional params: got %d, want 7", v)
	}
}

// TestUnusedParamsDDLAndDML runs DDL and DML statements with parameters
// they do not reference. CREATE TABLE is included because its schema SQL
// is executed with the statement arguments and never has placeholders.
func TestUnusedParamsDDLAndDML(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("googlesqlite", ":memory:?_test=unused_params_dml")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	for _, step := range []struct {
		query string
		args  []any
	}{
		{"CREATE TABLE up_dml (a INT64)", []any{sql.Named("unused", int64(1))}},
		{"INSERT INTO up_dml (a) VALUES (1)", []any{sql.Named("unused", int64(1))}},
		{"INSERT INTO up_dml (a) VALUES (2)", []any{int64(99)}},
		{"UPDATE up_dml SET a = a + 10 WHERE a = @a", []any{sql.Named("a", int64(1)), sql.Named("b", "x")}},
		{"DELETE FROM up_dml WHERE a = 2", []any{sql.Named("unused", int64(1))}},
	} {
		if _, err := db.ExecContext(ctx, step.query, step.args...); err != nil {
			t.Fatalf("%s: %v", step.query, err)
		}
	}

	var count, sum int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*), SUM(a) FROM up_dml").Scan(&count, &sum); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if count != 1 || sum != 11 {
		t.Fatalf("got (count, sum) = (%d, %d), want (1, 11)", count, sum)
	}
}

// TestUnusedParamsInScript runs multi-statement scripts with extra
// parameters. Each script ends with its only SELECT, because a SELECT in
// a non-last position is a separate known issue.
func TestUnusedParamsInScript(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("googlesqlite", ":memory:?_test=unused_params_script")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, "CREATE TABLE up_s (a INT64)"); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	for _, tc := range []struct {
		name  string
		query string
		args  []any
		want  int64
	}{
		{
			name:  "named",
			query: "INSERT INTO up_s (a) VALUES (1); SELECT COUNT(*) FROM up_s WHERE a = @a",
			args:  []any{sql.Named("a", int64(1)), sql.Named("b", "x")},
			want:  1,
		},
		{
			name:  "positional",
			query: "INSERT INTO up_s (a) VALUES (2); SELECT COUNT(*) FROM up_s WHERE a = ?",
			args:  []any{int64(2), int64(99)},
			want:  1,
		},
		{
			name:  "all_literal",
			query: "INSERT INTO up_s (a) VALUES (3); SELECT COUNT(*) FROM up_s",
			args:  []any{sql.Named("a", int64(1))},
			want:  3,
		},
	} {
		var got int64
		if err := db.QueryRowContext(ctx, tc.query, tc.args...).Scan(&got); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestPreparedStmtUnusedParams covers the prepared statement path, which
// binds arguments through encodeOrPassArgs rather than getArgsFromParams.
func TestPreparedStmtUnusedParams(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("googlesqlite", ":memory:?_test=unused_params_prepared")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()

	queryOne := func(query string, args ...any) int64 {
		t.Helper()
		stmt, err := conn.PrepareContext(ctx, query)
		if err != nil {
			t.Fatalf("Prepare %q: %v", query, err)
		}
		defer stmt.Close()
		rows, err := stmt.QueryContext(ctx, args...)
		if err != nil {
			t.Fatalf("Query %q: %v", query, err)
		}
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("Scan %q: %v", query, err)
			}
			got = append(got, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows %q: %v", query, err)
		}
		if len(got) != 1 {
			t.Fatalf("%q: got %d rows, want 1", query, len(got))
		}
		return got[0]
	}

	if got := queryOne("SELECT 1 AS v", int64(5)); got != 1 {
		t.Fatalf("SELECT 1 with unused arg: got %d, want 1", got)
	}
	if got := queryOne("SELECT ? AS v", int64(42), int64(43)); got != 42 {
		t.Fatalf("SELECT ? with extra arg: got %d, want 42", got)
	}

	if _, err := conn.ExecContext(ctx, "CREATE TABLE up_p (a INT64)"); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	func() {
		stmt, err := conn.PrepareContext(ctx, "INSERT INTO up_p (a) VALUES (1)")
		if err != nil {
			t.Fatalf("Prepare INSERT: %v", err)
		}
		defer stmt.Close()
		if _, err := stmt.ExecContext(ctx, int64(9)); err != nil {
			t.Fatalf("Exec INSERT with unused arg: %v", err)
		}
	}()
	if got := queryOne("SELECT COUNT(*) FROM up_p"); got != 1 {
		t.Fatalf("COUNT(*): got %d, want 1", got)
	}
}

// TestMissingUsedParamErrors checks that ignoring unused parameters does
// not hide a parameter the statement uses but the caller did not supply.
func TestMissingUsedParamErrors(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("googlesqlite", ":memory:?_test=unused_params_missing")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	var a, b any
	err = db.QueryRowContext(ctx, "SELECT @a, @b", sql.Named("b", int64(1))).Scan(&a, &b)
	if err == nil || !strings.Contains(err.Error(), "not enough query arguments") {
		t.Fatalf("expected an error containing %q, got %v", "not enough query arguments", err)
	}
}

// TestScriptVariableParams checks that query parameters on the
// right-hand side of a DECLARE DEFAULT or SET are bound when the
// variable is evaluated, instead of evaluating to NULL. Positional
// values are consumed in the order their `?` appears in the script.
func TestScriptVariableParams(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("googlesqlite", ":memory:?_test=unused_params_script_vars")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		query string
		args  []any
		want  []any
	}{
		{
			name:  "declare_named",
			query: "DECLARE x INT64 DEFAULT @p; SELECT x",
			args:  []any{sql.Named("p", int64(4)), sql.Named("q", int64(1))},
			want:  []any{int64(4)},
		},
		{
			name:  "set_named",
			query: "DECLARE x INT64 DEFAULT 1; SET x = @p; SELECT x",
			args:  []any{sql.Named("p", int64(4))},
			want:  []any{int64(4)},
		},
		{
			name:  "set_named_repeated",
			query: "DECLARE x INT64 DEFAULT @p; SET x = x + @p; SELECT x, @q",
			args:  []any{sql.Named("p", int64(4)), sql.Named("q", int64(1))},
			want:  []any{int64(8), int64(1)},
		},
		{
			name:  "declare_named_string",
			query: "DECLARE s STRING DEFAULT @s; SELECT s",
			args:  []any{sql.Named("s", "a\\b\"c\nd'e")},
			want:  []any{"a\\b\"c\nd'e"},
		},
		{
			name:  "declare_positional",
			query: "DECLARE d INT64 DEFAULT ?; SELECT d",
			args:  []any{int64(4)},
			want:  []any{int64(4)},
		},
		{
			name:  "declare_positional_order",
			query: "DECLARE d INT64 DEFAULT ?; SELECT ?, d",
			args:  []any{int64(4), int64(5)},
			want:  []any{int64(5), int64(4)},
		},
		{
			name:  "declare_positional_extra",
			query: "DECLARE d INT64 DEFAULT ?; SELECT ?",
			args:  []any{int64(1), int64(2), int64(3)},
			want:  []any{int64(2)},
		},
		{
			name:  "declare_positional_string",
			query: "DECLARE s STRING DEFAULT ?; SELECT s",
			args:  []any{"hi"},
			want:  []any{"hi"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := make([]any, len(tc.want))
			ptrs := make([]any, len(got))
			for i := range got {
				ptrs[i] = &got[i]
			}
			if err := db.QueryRowContext(ctx, tc.query, tc.args...).Scan(ptrs...); err != nil {
				t.Fatalf("%s: %v", tc.query, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%s: got %#v, want %#v", tc.query, got, tc.want)
			}
		})
	}

	// A DECLARE referencing a parameter the caller did not supply must
	// fail rather than silently evaluate to NULL.
	var v any
	err = db.QueryRowContext(ctx, "DECLARE m INT64 DEFAULT @missing; SELECT m").Scan(&v)
	if err == nil || !strings.Contains(err.Error(), "not enough query arguments") {
		t.Fatalf("missing parameter: expected an error containing %q, got %v (value %v)", "not enough query arguments", err, v)
	}
}
