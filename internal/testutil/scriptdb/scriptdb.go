// Package scriptdb 仅为单元测试提供确定性 SQL 驱动。
// 它验证应用查询和事务边界，不验证 MySQL 行为。
package scriptdb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

type Step struct {
	Kind, SQL string
	Rows      [][]driver.Value
	Columns   int
	Names     []string
	Err       error
	After     func()
	// Args 非空时收集该步骤实际绑定的参数，用于断言业务日历等绑定值。
	Args *[]driver.NamedValue
	// SQLOut 非空时记录该步骤匹配到的完整 SQL，用于断言占位符与参数数量一致。
	SQLOut *string
}

type connector struct {
	t     *testing.T
	steps []Step
}

// Open 核验受控SQL脚本的连接、事务或结果边界。
// 参数：当前测试的上下文、SQL参数或结果行缓冲；无入参方法为无。
// 返回值：预设的驱动结果或错误，不连接真实数据库。
func Open(t *testing.T, steps ...Step) *sql.DB {
	t.Helper()
	c := &connector{t: t, steps: steps}
	db := sql.OpenDB(c)
	t.Cleanup(func() {
		_ = db.Close()
		if len(c.steps) != 0 {
			t.Errorf("unexecuted SQL boundary: %s %s", c.steps[0].Kind, c.steps[0].SQL)
		}
	})
	return db
}

// take 核验受控SQL脚本的连接、事务或结果边界。
// 参数：当前测试的上下文、SQL参数或结果行缓冲；无入参方法为无。
// 返回值：预设的驱动结果或错误，不连接真实数据库。
func (c *connector) take(kind, query string, args []driver.NamedValue) (Step, error) {
	if len(c.steps) == 0 {
		err := fmt.Errorf("unexpected %s: %s", kind, query)
		c.t.Error(err)
		return Step{}, err
	}
	s := c.steps[0]
	if kind != s.Kind || !strings.Contains(query, s.SQL) {
		err := fmt.Errorf("got %s %s; want %s containing %s", kind, query, s.Kind, s.SQL)
		c.t.Error(err)
		return Step{}, err
	}
	c.steps = c.steps[1:]
	if s.Args != nil {
		*s.Args = append(*s.Args, args...)
	}
	if s.SQLOut != nil {
		*s.SQLOut = query
	}
	if s.After != nil {
		s.After()
	}
	return s, s.Err
}

// Connect 核验受控SQL脚本的连接、事务或结果边界。
// 参数：当前测试的上下文、SQL参数或结果行缓冲；无入参方法为无。
// 返回值：预设的驱动结果或错误，不连接真实数据库。
func (c *connector) Connect(context.Context) (driver.Conn, error) { return &conn{c}, nil }

// Driver 核验受控SQL脚本的连接、事务或结果边界。
// 参数：当前测试的上下文、SQL参数或结果行缓冲；无入参方法为无。
// 返回值：预设的驱动结果或错误，不连接真实数据库。
func (c *connector) Driver() driver.Driver { return drv{} }

type drv struct{}

// Open 核验受控SQL脚本的连接、事务或结果边界。
// 参数：当前测试的上下文、SQL参数或结果行缓冲；无入参方法为无。
// 返回值：预设的驱动结果或错误，不连接真实数据库。
func (drv) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }

type conn struct{ c *connector }

// Prepare 核验受控SQL脚本的连接、事务或结果边界。
// 参数：当前测试的上下文、SQL参数或结果行缓冲；无入参方法为无。
// 返回值：预设的驱动结果或错误，不连接真实数据库。
func (c *conn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unexpected prepare") }

// Close 核验受控SQL脚本的连接、事务或结果边界。
// 参数：当前测试的上下文、SQL参数或结果行缓冲；无入参方法为无。
// 返回值：预设的驱动结果或错误，不连接真实数据库。
func (c *conn) Close() error { return nil }

// Begin 核验受控SQL脚本的连接、事务或结果边界。
// 参数：当前测试的上下文、SQL参数或结果行缓冲；无入参方法为无。
// 返回值：预设的驱动结果或错误，不连接真实数据库。
func (c *conn) Begin() (driver.Tx, error) {
	_, err := c.c.take("begin", "", nil)
	return c, err
}

// BeginTx 核验受控SQL脚本的连接、事务或结果边界。
// 参数：当前测试的上下文、SQL参数或结果行缓冲；无入参方法为无。
// 返回值：预设的驱动结果或错误，不连接真实数据库。
func (c *conn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) { return c.Begin() }

// Commit 核验受控SQL脚本的连接、事务或结果边界。
// 参数：当前测试的上下文、SQL参数或结果行缓冲；无入参方法为无。
// 返回值：预设的驱动结果或错误，不连接真实数据库。
func (c *conn) Commit() error { _, err := c.c.take("commit", "", nil); return err }

// Rollback 核验受控SQL脚本的连接、事务或结果边界。
// 参数：当前测试的上下文、SQL参数或结果行缓冲；无入参方法为无。
// 返回值：预设的驱动结果或错误，不连接真实数据库。
func (c *conn) Rollback() error {
	_, err := c.c.take("rollback", "", nil)
	return err
}

// ExecContext 核验受控SQL脚本的连接、事务或结果边界。
// 参数：当前测试的上下文、SQL参数或结果行缓冲；无入参方法为无。
// 返回值：预设的驱动结果或错误，不连接真实数据库。
func (c *conn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	_, err := c.c.take("exec", q, args)
	return driver.RowsAffected(1), err
}

// QueryContext 核验受控SQL脚本的连接、事务或结果边界。
// 参数：当前测试的上下文、SQL参数或结果行缓冲；无入参方法为无。
// 返回值：预设的驱动结果或错误，不连接真实数据库。
func (c *conn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	s, err := c.c.take("query", q, args)
	return &rows{values: s.Rows, columns: s.Columns, names: s.Names}, err
}

type rows struct {
	values  [][]driver.Value
	columns int
	names   []string
}

// Columns 核验受控SQL脚本的连接、事务或结果边界。
// 参数：当前测试的上下文、SQL参数或结果行缓冲；无入参方法为无。
// 返回值：预设的驱动结果或错误，不连接真实数据库。
func (r *rows) Columns() []string {
	if r.names != nil {
		return r.names
	}
	n := r.columns
	if len(r.values) > 0 {
		n = len(r.values[0])
	}
	return make([]string, n)
}

// Close 核验受控SQL脚本的连接、事务或结果边界。
// 参数：当前测试的上下文、SQL参数或结果行缓冲；无入参方法为无。
// 返回值：预设的驱动结果或错误，不连接真实数据库。
func (r *rows) Close() error { return nil }

// Next 核验受控SQL脚本的连接、事务或结果边界。
// 参数：当前测试的上下文、SQL参数或结果行缓冲；无入参方法为无。
// 返回值：预设的驱动结果或错误，不连接真实数据库。
func (r *rows) Next(dest []driver.Value) error {
	if len(r.values) == 0 {
		return io.EOF
	}
	copy(dest, r.values[0])
	r.values = r.values[1:]
	return nil
}
