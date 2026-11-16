package internal

import (
	"context"
	"database/sql/driver"
	"errors"
	"sync"

	googlesql "github.com/goccy/go-googlesql"
)

// ErrStatementNotHandled is returned by a statement handler that declines a
// statement; the engine then runs its built-in action instead.
var ErrStatementNotHandled = errors.New("googlesqlite: statement not handled")

// HandlerStatement is a statement handed to the statement hook.
type HandlerStatement struct {
	Kind     googlesql.ResolvedNodeKind
	Node     googlesql.ResolvedStatementNode
	SQL      string
	InScript bool
	// Changed are the catalog changes of the statements of the query that
	// ran before this one.
	Changed *ChangedCatalog
}

// StatementHook runs a statement in place of its built-in action. conn is
// the connection the statement runs on.
type StatementHook func(ctx context.Context, conn *Conn, stmt *HandlerStatement) error

var (
	statementHookMu sync.RWMutex
	statementHooks  = map[googlesql.ResolvedNodeKind]StatementHook{}
)

// SetStatementHook installs hook for statements of kind. A nil hook removes
// it.
func SetStatementHook(kind googlesql.ResolvedNodeKind, hook StatementHook) {
	statementHookMu.Lock()
	defer statementHookMu.Unlock()
	if hook == nil {
		delete(statementHooks, kind)
		return
	}
	statementHooks[kind] = hook
}

func statementHookFor(kind googlesql.ResolvedNodeKind) StatementHook {
	statementHookMu.RLock()
	defer statementHookMu.RUnlock()
	return statementHooks[kind]
}

// statementSource is the text of the statement being analyzed, and whether
// it is one of several statements of the query.
type statementSource struct {
	sql      string
	inScript bool
}

type statementSourceKey struct{}

func withStatementSource(ctx context.Context, src statementSource) context.Context {
	return context.WithValue(ctx, statementSourceKey{}, src)
}

func statementSourceFromContext(ctx context.Context) statementSource {
	src, _ := ctx.Value(statementSourceKey{}).(statementSource)
	return src
}

// statementText returns the text of stmt in query, or query itself when the
// location of stmt is unknown.
func statementText(stmt googlesql.ASTStatementNode, query string) string {
	start, err := stmt.StartLocation()
	if err != nil || start == nil {
		return query
	}
	end, err := stmt.EndLocation()
	if err != nil || end == nil {
		return query
	}
	from, err := start.GetByteOffset()
	if err != nil {
		return query
	}
	to, err := end.GetByteOffset()
	if err != nil || from < 0 || to < from || int(to) > len(query) {
		return query
	}
	return query[from:to]
}

// handlerStmtAction runs a statement through its statement hook. When the
// hook declines the statement (ErrStatementNotHandled), the built-in action
// runs instead.
type handlerStmtAction struct {
	hook     StatementHook
	stmt     *HandlerStatement
	builtin  func() (StmtAction, error)
	fallback StmtAction
}

func (a *handlerStmtAction) builtinAction() (StmtAction, error) {
	if a.fallback == nil {
		action, err := a.builtin()
		if err != nil {
			return nil, err
		}
		a.fallback = action
	}
	return a.fallback, nil
}

// run calls the hook and reports whether it handled the statement.
func (a *handlerStmtAction) run(ctx context.Context, conn *Conn) (bool, error) {
	stmt := *a.stmt
	stmt.Changed = conn.cc.clone()
	err := a.hook(ctx, conn, &stmt)
	if errors.Is(err, ErrStatementNotHandled) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Prepare runs the built-in action: a prepared statement may run many times
// and cannot hand its statement to a hook only once.
func (a *handlerStmtAction) Prepare(ctx context.Context, conn *Conn) (driver.Stmt, error) {
	action, err := a.builtinAction()
	if err != nil {
		return nil, err
	}
	return action.Prepare(ctx, conn)
}

func (a *handlerStmtAction) ExecContext(ctx context.Context, conn *Conn) (driver.Result, error) {
	handled, err := a.run(ctx, conn)
	if err != nil {
		return nil, err
	}
	if handled {
		return &Result{conn: conn}, nil
	}
	action, err := a.builtinAction()
	if err != nil {
		return nil, err
	}
	return action.ExecContext(ctx, conn)
}

func (a *handlerStmtAction) QueryContext(ctx context.Context, conn *Conn) (*Rows, error) {
	handled, err := a.run(ctx, conn)
	if err != nil {
		return nil, err
	}
	if handled {
		return &Rows{conn: conn}, nil
	}
	action, err := a.builtinAction()
	if err != nil {
		return nil, err
	}
	return action.QueryContext(ctx, conn)
}

func (a *handlerStmtAction) Cleanup(ctx context.Context, conn *Conn) error {
	if a.fallback == nil {
		return nil
	}
	return a.fallback.Cleanup(ctx, conn)
}

func (a *handlerStmtAction) Args() []any {
	if a.fallback == nil {
		return nil
	}
	return a.fallback.Args()
}

// MergeChangedCatalog records the catalog changes of other, which happened
// after those of c, as changes of c. Within other, a table is either added or
// deleted, so its deletions go first: a table deleted by c and added again by
// other is then added.
func (c *Conn) MergeChangedCatalog(other *ChangedCatalog) {
	if other == nil {
		return
	}
	if other.Table != nil {
		for _, spec := range other.Table.Deleted {
			c.deleteTable(spec)
		}
		for _, spec := range other.Table.Added {
			c.addTable(spec)
		}
		for _, spec := range other.Table.Updated {
			c.updateTable(spec)
		}
	}
	if other.Function != nil {
		for _, spec := range other.Function.Added {
			c.addFunction(spec)
		}
		for _, spec := range other.Function.Deleted {
			c.deleteFunction(spec)
		}
	}
}

// clone returns a copy of c that later changes of c do not affect.
func (c *ChangedCatalog) clone() *ChangedCatalog {
	return &ChangedCatalog{
		Table: &ChangedTable{
			Added:   append([]*TableSpec{}, c.Table.Added...),
			Updated: append([]*TableSpec{}, c.Table.Updated...),
			Deleted: append([]*TableSpec{}, c.Table.Deleted...),
		},
		Function: &ChangedFunction{
			Added:   append([]*FunctionSpec{}, c.Function.Added...),
			Deleted: append([]*FunctionSpec{}, c.Function.Deleted...),
		},
	}
}

// ChangedCatalog returns the catalog changes recorded on c so far.
func (c *Conn) ChangedCatalog() *ChangedCatalog {
	return c.cc
}

// SetOwner attaches the driver connection that created c.
func (c *Conn) SetOwner(owner any) {
	c.owner = owner
}

// Owner returns the driver connection that created c.
func (c *Conn) Owner() any {
	return c.owner
}
