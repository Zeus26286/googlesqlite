package googlesqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"

	googlesql "github.com/goccy/go-googlesql"

	"github.com/goccy/googlesqlite/internal"
)

// ErrStatementNotHandled is returned by a StatementHandler that declines a
// statement. The engine then runs the statement with its built-in action, as
// if no handler were registered.
var ErrStatementNotHandled = internal.ErrStatementNotHandled

// Statement is a statement handed to a StatementHandler.
type Statement struct {
	// Kind is the kind of the resolved statement.
	Kind googlesql.ResolvedNodeKind
	// Node is the resolved statement.
	Node googlesql.ResolvedStatementNode
	// SQL is the text of this statement alone, as the engine runs it:
	// within a script, script variables are already substituted.
	SQL string
	// InScript reports whether the statement is one of several
	// statements of the query.
	InScript bool
	// Changed are the catalog changes of the statements of the query that
	// ran before this one, which the catalog changes of the query will
	// include. The handler must not modify them.
	Changed *ChangedCatalog
}

// StatementHandler runs a statement in place of the engine's built-in action,
// standalone or at its place in a multi-statement script.
//
// conn runs on the same SQLite connection as the statement, inside the
// transaction the statement runs in, so it sees what the statements before it
// did and its changes commit or roll back with them. When the handler
// succeeds, the catalog changes of the statements it ran through conn are
// reported as changes of the query, except those of rolled back
// transactions; when it fails, none are.
// BeginTx on conn starts a SAVEPOINT; the handler must commit or roll back
// every transaction it starts. conn is valid only until the handler returns.
//
// Returning ErrStatementNotHandled, before running anything through conn,
// hands the statement back to the built-in action. Any other error fails the
// statement, which stops a script there.
type StatementHandler func(ctx context.Context, conn *sql.Conn, stmt *Statement) error

var (
	statementHandlersMu sync.Mutex
	statementHandlers   = map[googlesql.ResolvedNodeKind]StatementHandler{}
)

// RegisterStatementHandler makes the engine call h for statements of kind.
// Registering a kind again replaces its handler; a nil h removes it, which
// restores the built-in action. Handlers apply to every googlesqlite database
// of the process.
func RegisterStatementHandler(kind googlesql.ResolvedNodeKind, h StatementHandler) {
	statementHandlersMu.Lock()
	defer statementHandlersMu.Unlock()
	if h == nil {
		delete(statementHandlers, kind)
		internal.SetStatementHook(kind, nil)
		return
	}
	statementHandlers[kind] = h
	internal.SetStatementHook(kind, func(ctx context.Context, conn *internal.Conn, stmt *internal.HandlerStatement) error {
		return runStatementHandler(ctx, h, conn, stmt)
	})
}

// nestedState is the state of a Conn handed to a StatementHandler.
type nestedState struct {
	// parent is the connection of the statement being handled; the catalog
	// changes made through the nested Conn are merged into it when the
	// handler succeeds.
	parent *internal.Conn
	// changes are the catalog changes of the queries run through the
	// nested Conn, in order.
	changes []*internal.ChangedCatalog
	// savepoints are the SAVEPOINTs of the transactions started through
	// the nested Conn that are still open, innermost last.
	savepoints []savepoint
	seq        int
}

type savepoint struct {
	name string
	// mark is the number of changes when the savepoint started; rolling
	// back to it drops the changes after it.
	mark int
}

func runStatementHandler(ctx context.Context, h StatementHandler, parent *internal.Conn, stmt *internal.HandlerStatement) (e error) {
	owner, ok := parent.Owner().(*Conn)
	if !ok {
		return fmt.Errorf("googlesqlite: statement handler: connection of the statement is unknown")
	}
	nested, err := owner.nestedConn(parent)
	if err != nil {
		return err
	}
	db := sql.OpenDB(&nestedConnector{conn: nested})
	db.SetMaxOpenConns(1)
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("googlesqlite: statement handler: %w", err)
	}
	err = h(ctx, conn, &Statement{Kind: stmt.Kind, Node: stmt.Node, SQL: stmt.SQL, InScript: stmt.InScript, Changed: stmt.Changed})
	if len(nested.nested.savepoints) != 0 {
		// The handler left a transaction open, which also keeps conn
		// from closing: roll the transaction back directly.
		rollbackErr := nested.rollbackAllSavepoints()
		if err == nil {
			err = errors.Join(fmt.Errorf("googlesqlite: statement handler left a transaction open"), rollbackErr)
		}
		return err
	}
	if closeErr := conn.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	for _, changed := range nested.nested.changes {
		parent.MergeChangedCatalog(changed)
	}
	return nil
}

// nestedConn returns a Conn that runs on the SQLite connection of c, for a
// StatementHandler of a statement running on parent.
func (c *Conn) nestedConn(parent *internal.Conn) (*Conn, error) {
	if c.handlerAnalyzer == nil {
		analyzer, err := internal.NewAnalyzer(c.catalog)
		if err != nil {
			return nil, fmt.Errorf("googlesqlite: statement handler: failed to create analyzer: %w", err)
		}
		c.handlerAnalyzer = analyzer
	}
	return &Conn{
		conn:           c.conn,
		txLock:         c.txLock,
		analyzer:       c.handlerAnalyzer,
		catalog:        c.catalog,
		systemVars:     map[string]string{},
		scriptVars:     map[string]string{},
		materializeCTE: c.materializeCTE,
		nested:         &nestedState{parent: parent},
	}, nil
}

// mergeChangedCatalog records the catalog changes of a query run through a
// nested Conn, for the statement being handled.
func (c *Conn) mergeChangedCatalog(conn *internal.Conn) {
	if c.nested == nil || conn == nil || !conn.ChangedCatalog().Changed() {
		return
	}
	c.nested.changes = append(c.nested.changes, conn.ChangedCatalog())
}

func (c *Conn) beginSavepoint(ctx context.Context) (driver.Tx, error) {
	c.nested.seq++
	name := fmt.Sprintf("googlesqlite_handler_%d", c.nested.seq)
	if _, err := c.conn.ExecContext(ctx, "SAVEPOINT "+name); err != nil {
		return nil, err
	}
	c.nested.savepoints = append(c.nested.savepoints, savepoint{name: name, mark: len(c.nested.changes)})
	return &Tx{conn: c, savepoint: name}, nil
}

// endSavepoint releases the SAVEPOINT name, first rolling back to it unless
// commit is set.
func (c *Conn) endSavepoint(name string, commit bool) error {
	savepoints := c.nested.savepoints
	if len(savepoints) == 0 || savepoints[len(savepoints)-1].name != name {
		return sql.ErrTxDone
	}
	sp := savepoints[len(savepoints)-1]
	c.nested.savepoints = savepoints[:len(savepoints)-1]
	query := "RELEASE SAVEPOINT " + name
	if !commit {
		query = "ROLLBACK TO SAVEPOINT " + name + "; " + query
		c.nested.changes = c.nested.changes[:sp.mark]
	}
	_, err := c.conn.ExecContext(context.Background(), query)
	return err
}

func (c *Conn) rollbackAllSavepoints() error {
	var errs []error
	for len(c.nested.savepoints) != 0 {
		name := c.nested.savepoints[len(c.nested.savepoints)-1].name
		errs = append(errs, c.endSavepoint(name, false))
	}
	return errors.Join(errs...)
}

// nestedConnector hands out the Conn of a StatementHandler.
type nestedConnector struct {
	conn *Conn
}

func (c *nestedConnector) Connect(context.Context) (driver.Conn, error) {
	return c.conn, nil
}

func (c *nestedConnector) Driver() driver.Driver {
	return &Driver{}
}
