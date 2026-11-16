package googlesqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	googlesql "github.com/goccy/go-googlesql"

	"github.com/goccy/googlesqlite"
)

// registerHandler registers h for kind for the duration of the test.
func registerHandler(t *testing.T, kind googlesql.ResolvedNodeKind, h googlesqlite.StatementHandler) {
	t.Helper()
	googlesqlite.RegisterStatementHandler(kind, h)
	t.Cleanup(func() { googlesqlite.RegisterStatementHandler(kind, nil) })
}

func openHandlerTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("googlesqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// addColumnB rebuilds table t with a column b, in a transaction of conn.
func addColumnB(ctx context.Context, conn *sql.Conn) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, query := range []string{
		"CREATE TABLE t_tmp AS SELECT a, CAST(NULL AS INT64) AS b FROM t",
		"DROP TABLE t",
		"CREATE TABLE t AS SELECT * FROM t_tmp",
		"DROP TABLE t_tmp",
	} {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func TestStatementHandlerRunsInScript(t *testing.T) {
	ctx := context.Background()
	db := openHandlerTestDB(t)
	type ctxKey struct{}
	var got []*googlesqlite.Statement
	registerHandler(t, googlesql.ResolvedNodeKindResolvedAlterTableStmt, func(ctx context.Context, conn *sql.Conn, stmt *googlesqlite.Statement) error {
		if ctx.Value(ctxKey{}) != "job" {
			return errors.New("handler did not get the caller's context")
		}
		got = append(got, stmt)
		return addColumnB(ctx, conn)
	})

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(context.WithValue(ctx, ctxKey{}, "job"), `
CREATE TABLE t (a INT64);
INSERT INTO t (a) VALUES (1);
ALTER TABLE t ADD COLUMN b INT64;
INSERT INTO t (a, b) VALUES (2, 20);
SELECT a, b FROM t ORDER BY a`)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		a int64
		b sql.NullInt64
	}
	var result []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.a, &r.b); err != nil {
			t.Fatal(err)
		}
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	changed, err := googlesqlite.ChangedCatalogFromRows(rows)
	if err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if len(result) != 2 || result[0].a != 1 || result[0].b.Valid || result[1].a != 2 || result[1].b.Int64 != 20 {
		t.Fatalf("unexpected rows %+v", result)
	}
	if len(got) != 1 {
		t.Fatalf("handler called %d times, want 1", len(got))
	}
	if got[0].SQL != "ALTER TABLE t ADD COLUMN b INT64" || !got[0].InScript {
		t.Fatalf("unexpected statement %q (in script %v)", got[0].SQL, got[0].InScript)
	}
	// The handler sees the table the script created before it.
	if before := got[0].Changed.Table.Added; len(before) != 1 || before[0].TableName() != "t" || len(before[0].Columns) != 1 {
		t.Fatalf("changes before the statement: %+v", got[0].Changed.Table)
	}
	// The table rebuilt by the handler is reported as a change of the
	// query, and the handler's temporary table is not.
	var added []string
	for _, spec := range changed.Table.Added {
		added = append(added, spec.TableName())
	}
	if strings.Join(added, ",") != "t" {
		t.Fatalf("added tables %v, want [t]", added)
	}
	var columns []string
	for _, c := range changed.Table.Added[0].Columns {
		columns = append(columns, c.Name)
	}
	if strings.Join(columns, ",") != "a,b" {
		t.Fatalf("columns of t %v, want [a b]", columns)
	}
}

func TestStatementHandlerErrorStopsScript(t *testing.T) {
	ctx := context.Background()
	db := openHandlerTestDB(t)
	registerHandler(t, googlesql.ResolvedNodeKindResolvedAlterTableStmt, func(ctx context.Context, conn *sql.Conn, stmt *googlesqlite.Statement) error {
		return errors.New("alter failed")
	})
	if _, err := db.ExecContext(ctx, "CREATE TABLE t (a INT64)"); err != nil {
		t.Fatal(err)
	}
	_, err := db.ExecContext(ctx, `
ALTER TABLE t ADD COLUMN b INT64;
INSERT INTO t (a) VALUES (1)`)
	if err == nil || !strings.Contains(err.Error(), "alter failed") {
		t.Fatalf("got error %v, want alter failed", err)
	}
	var n int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the statement after the failed one ran: %d rows", n)
	}
}

func TestStatementHandlerNotHandledRunsBuiltin(t *testing.T) {
	ctx := context.Background()
	db := openHandlerTestDB(t)
	calls := 0
	registerHandler(t, googlesql.ResolvedNodeKindResolvedCreateTableStmt, func(ctx context.Context, conn *sql.Conn, stmt *googlesqlite.Statement) error {
		calls++
		return googlesqlite.ErrStatementNotHandled
	})
	if _, err := db.ExecContext(ctx, "CREATE TABLE t (a INT64); INSERT INTO t (a) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || n != 1 {
		t.Fatalf("calls = %d, rows = %d; want 1 and 1", calls, n)
	}
}

func TestStatementHandlerOpenTransactionIsRolledBack(t *testing.T) {
	ctx := context.Background()
	db := openHandlerTestDB(t)
	if _, err := db.ExecContext(ctx, "CREATE TABLE t (a INT64)"); err != nil {
		t.Fatal(err)
	}
	registerHandler(t, googlesql.ResolvedNodeKindResolvedAlterTableStmt, func(ctx context.Context, conn *sql.Conn, stmt *googlesqlite.Statement) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO t (a) VALUES (1)")
		return err
	})
	_, err := db.ExecContext(ctx, "ALTER TABLE t ADD COLUMN b INT64")
	if err == nil || !strings.Contains(err.Error(), "left a transaction open") {
		t.Fatalf("got error %v, want a transaction left open", err)
	}
	var n int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the open transaction was not rolled back: %d rows", n)
	}
}

func TestStatementHandlerSavepointRollback(t *testing.T) {
	ctx := context.Background()
	db := openHandlerTestDB(t)
	if _, err := db.ExecContext(ctx, "CREATE TABLE t (a INT64)"); err != nil {
		t.Fatal(err)
	}
	registerHandler(t, googlesql.ResolvedNodeKindResolvedAlterTableStmt, func(ctx context.Context, conn *sql.Conn, stmt *googlesqlite.Statement) error {
		if _, err := conn.ExecContext(ctx, "INSERT INTO t (a) VALUES (1)"); err != nil {
			return err
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO t (a) VALUES (2)"); err != nil {
			return err
		}
		return tx.Rollback()
	})
	if _, err := db.ExecContext(ctx, "ALTER TABLE t ADD COLUMN b INT64"); err != nil {
		t.Fatal(err)
	}
	var got []int64
	rows, err := db.QueryContext(ctx, "SELECT a FROM t ORDER BY a")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var a int64
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		got = append(got, a)
	}
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("rows %v, want [1]: the savepoint should undo only its own insert", got)
	}
}

func TestCreateTableLikeWithoutHandler(t *testing.T) {
	ctx := context.Background()
	db := openHandlerTestDB(t)
	if _, err := db.ExecContext(ctx, `
CREATE TABLE src (a INT64, b STRING);
CREATE TABLE dst LIKE src;
INSERT INTO dst (a, b) VALUES (1, 'x')`); err != nil {
		t.Fatal(err)
	}
	var (
		a int64
		b string
	)
	if err := db.QueryRowContext(ctx, "SELECT a, b FROM dst").Scan(&a, &b); err != nil {
		t.Fatal(err)
	}
	if a != 1 || b != "x" {
		t.Fatalf("got (%d, %q)", a, b)
	}
}

func TestStatementHandlerSeesLikeTable(t *testing.T) {
	ctx := context.Background()
	db := openHandlerTestDB(t)
	var likeSQL []string
	registerHandler(t, googlesql.ResolvedNodeKindResolvedCreateTableStmt, func(ctx context.Context, conn *sql.Conn, stmt *googlesqlite.Statement) error {
		node := stmt.Node.(*googlesql.ResolvedCreateTableStmt)
		if like, _ := node.LikeTable(); like == nil {
			return googlesqlite.ErrStatementNotHandled
		}
		likeSQL = append(likeSQL, stmt.SQL)
		_, err := conn.ExecContext(ctx, "CREATE TABLE dst (a INT64, b STRING, c BOOL)")
		return err
	})
	if _, err := db.ExecContext(ctx, `
CREATE TABLE src (a INT64, b STRING);
CREATE TABLE dst LIKE src;
INSERT INTO dst (a, b, c) VALUES (1, 'x', TRUE)`); err != nil {
		t.Fatal(err)
	}
	if len(likeSQL) != 1 || likeSQL[0] != "CREATE TABLE dst LIKE src" {
		t.Fatalf("handler saw %q", likeSQL)
	}
}

func TestStatementHandlerReportsCatalogChangesOnlyOnSuccess(t *testing.T) {
	ctx := context.Background()
	db := openHandlerTestDB(t)
	if _, err := db.ExecContext(ctx, "CREATE TABLE t (a INT64)"); err != nil {
		t.Fatal(err)
	}
	fail := true
	call := 0
	registerHandler(t, googlesql.ResolvedNodeKindResolvedAlterTableStmt, func(ctx context.Context, conn *sql.Conn, stmt *googlesqlite.Statement) error {
		call++
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("CREATE TABLE kept%d (a INT64)", call)); err != nil {
			return err
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("CREATE TABLE rolled_back%d (a INT64)", call)); err != nil {
			return err
		}
		if err := tx.Rollback(); err != nil {
			return err
		}
		if fail {
			return errors.New("alter failed")
		}
		return nil
	})
	changedTables := func(sql string) ([]string, error) {
		rows, err := db.QueryContext(ctx, sql)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		changed, err := googlesqlite.ChangedCatalogFromRows(rows)
		if err != nil {
			return nil, err
		}
		var names []string
		for _, spec := range changed.Table.Added {
			names = append(names, spec.TableName())
		}
		return names, nil
	}
	if _, err := changedTables("ALTER TABLE t ADD COLUMN b INT64"); err == nil || !strings.Contains(err.Error(), "alter failed") {
		t.Fatalf("got error %v, want alter failed", err)
	}
	fail = false
	// A SELECT before the ALTER TABLE would leak its rows and keep the
	// database from closing, with or without a handler.
	got, err := changedTables("INSERT INTO t (a) VALUES (1); ALTER TABLE t ADD COLUMN b INT64")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "kept2" {
		t.Fatalf("added tables %v, want [kept2]", got)
	}
}
