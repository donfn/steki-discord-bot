package s3db

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"

	sqlite "modernc.org/sqlite"
)

// The driver wraps SQLite's and adds one thing: once a statement outside a
// transaction, or a transaction's commit, has finished, it waits for the shard's
// barrier. The store's SQL needs no changes and cannot forget to wait.

var baseDriver = &sqlite.Driver{}

type connector struct {
	s   *Shard
	dsn string
}

func (c *connector) Connect(context.Context) (driver.Conn, error) {
	inner, err := baseDriver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &conn{inner: inner, s: c.s}, nil
}

func (c *connector) Driver() driver.Driver { return baseDriver }

type conn struct {
	inner driver.Conn
	s     *Shard
	inTx  bool
}

var (
	_ driver.ExecerContext      = (*conn)(nil)
	_ driver.QueryerContext     = (*conn)(nil)
	_ driver.ConnBeginTx        = (*conn)(nil)
	_ driver.ConnPrepareContext = (*conn)(nil)
	_ driver.Pinger             = (*conn)(nil)
	_ driver.SessionResetter    = (*conn)(nil)
	_ driver.Validator          = (*conn)(nil)
)

// settle waits for the barrier unless a transaction is open: its commit will.
func (c *conn) settle(ctx context.Context) error {
	if c.inTx {
		return nil
	}
	return c.s.barrier(ctx)
}

func (c *conn) settleNoContext() error {
	ctx, cancel := context.WithTimeout(context.Background(), commitTimeout)
	defer cancel()
	return c.settle(ctx)
}

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	st, err := c.inner.(driver.ConnPrepareContext).PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return &stmt{Stmt: st, c: c, write: !readOnly(query)}, nil
}

func (c *conn) Close() error { return c.inner.Close() }

func (c *conn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	inner, err := c.inner.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	c.inTx = true
	return &tx{inner: inner, c: c}, nil
}

func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	res, err := c.inner.(driver.ExecerContext).ExecContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	if err := c.settle(ctx); err != nil {
		return nil, err
	}
	return res, nil
}

func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	r, err := c.inner.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	return &rows{Rows: r, c: c, write: !readOnly(query)}, nil
}

func (c *conn) Ping(ctx context.Context) error { return c.inner.(driver.Pinger).Ping(ctx) }

func (c *conn) ResetSession(ctx context.Context) error {
	return c.inner.(driver.SessionResetter).ResetSession(ctx)
}

func (c *conn) IsValid() bool { return c.inner.(driver.Validator).IsValid() }

type tx struct {
	inner driver.Tx
	c     *conn
}

func (t *tx) Commit() error {
	err := t.inner.Commit()
	t.c.inTx = false
	if err != nil {
		return err
	}
	return t.c.settleNoContext()
}

func (t *tx) Rollback() error {
	t.c.inTx = false
	return t.inner.Rollback()
}

type stmt struct {
	driver.Stmt
	c     *conn
	write bool
}

func (s *stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	ex, ok := s.Stmt.(driver.StmtExecContext)
	if !ok {
		return nil, errors.New("s3db: statement cannot execute with a context")
	}
	res, err := ex.ExecContext(ctx, args)
	if err != nil {
		return nil, err
	}
	if err := s.c.settle(ctx); err != nil {
		return nil, err
	}
	return res, nil
}

func (s *stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := s.Stmt.(driver.StmtQueryContext)
	if !ok {
		return nil, errors.New("s3db: statement cannot query with a context")
	}
	r, err := q.QueryContext(ctx, args)
	if err != nil {
		return nil, err
	}
	return &rows{Rows: r, c: s.c, write: s.write}, nil
}

// rows settles on Close, since "insert … returning" commits only when its rows
// are finished. database/sql reports the error from Row.Scan and Rows.Close.
type rows struct {
	driver.Rows
	c     *conn
	write bool
}

func (r *rows) Close() error {
	if err := r.Rows.Close(); err != nil {
		return err
	}
	if !r.write {
		return nil
	}
	return r.c.settleNoContext()
}

// readOnly reports whether a query is a plain select. Reads never wait for the
// bucket, so they keep working while it is unreachable. Anything else that
// returns rows ("insert … returning") counts as a write.
func readOnly(query string) bool {
	q := strings.TrimLeft(query, " \t\r\n")
	return len(q) >= 6 && strings.EqualFold(q[:6], "select")
}
