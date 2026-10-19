package toolkit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"blog-my/internal/agent"
)

// sqlDB 由接线层在启动时注入（web.Setup 里从 model.DB 取）。
// 为 nil 时工具明确报“数据库未配置”，而不是 panic。
var sqlDB *sql.DB

// SetSQLDB 注入只读查询用的数据库句柄。传 nil 表示关闭该工具。
func SetSQLDB(db *sql.DB) { sqlDB = db }

// sqlDefaultRows / sqlMaxRows 控制返回行数上限，避免一次查询把上下文撑爆。
const (
	sqlDefaultRows = 50
	sqlMaxRows     = 200
)

// forbiddenSQL 是只读白名单之外的关键字。命中即拒绝，不做“先执行再看”的侥幸设计。
var forbiddenSQL = []string{
	"insert", "update", "delete", "drop", "alter", "create", "truncate", "replace",
	"grant", "revoke", "rename", "lock", "unlock", "call", "load", "outfile",
	"dumpfile", "set ", "commit", "rollback", "savepoint", "handler", "do ",
}

// SQLTool 执行只读 SQL 查询（SELECT / SHOW / DESCRIBE / EXPLAIN / WITH）。
//
// 安全边界是刻意的：这是**只读**工具，任何写操作与 DDL 一律拒绝。
// 模型要改数据时应该给出 SQL 让用户自己执行，而不是借这个工具落库。
type SQLTool struct{}

func (SQLTool) Name() string { return "sql_query" }

func (SQLTool) Description() string {
	return "对已配置的 MySQL 执行**只读**查询：SELECT / SHOW / DESCRIBE / EXPLAIN / WITH。" +
		"写操作与建表删表一律被拒绝。想看表结构先 SHOW TABLES，再 DESCRIBE 表名。"
}

func (SQLTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"sql":      map[string]any{"type": "string", "description": "只读 SQL 语句（单条）"},
			"max_rows": map[string]any{"type": "integer", "description": "最多返回多少行（默认 50，上限 200）"},
		},
		"required": []string{"sql"},
	}
}

func (SQLTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	var a struct {
		SQL     string `json:"sql"`
		MaxRows int    `json:"max_rows"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return agent.Result{}, fmt.Errorf("参数不是合法 JSON: %w", err)
	}
	q := strings.TrimSpace(a.SQL)
	if q == "" {
		return agent.Result{}, fmt.Errorf("sql 不能为空")
	}
	if err := sqlGuard(q); err != nil {
		return agent.Result{}, err
	}
	if sqlDB == nil {
		return agent.Result{}, fmt.Errorf("数据库未配置，无法执行查询")
	}

	maxRows := a.MaxRows
	if maxRows <= 0 {
		maxRows = sqlDefaultRows
	}
	if maxRows > sqlMaxRows {
		maxRows = sqlMaxRows
	}

	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	rows, err := sqlDB.QueryContext(runCtx, q)
	if err != nil {
		return agent.Result{}, fmt.Errorf("查询失败: %v", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return agent.Result{}, fmt.Errorf("读取列名失败: %v", err)
	}

	var (
		lines   []string
		count   int
		stopped bool
	)
	lines = append(lines, strings.Join(cols, "\t"))
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if count >= maxRows {
			stopped = true
			break
		}
		if err := rows.Scan(ptrs...); err != nil {
			return agent.Result{}, fmt.Errorf("读取结果失败: %v", err)
		}
		cells := make([]string, len(vals))
		for i, v := range vals {
			cells[i] = sqlCell(v)
		}
		lines = append(lines, strings.Join(cells, "\t"))
		count++
	}
	if err := rows.Err(); err != nil {
		return agent.Result{}, fmt.Errorf("遍历结果失败: %v", err)
	}

	text := strings.Join(lines, "\n")
	if stopped {
		text += fmt.Sprintf("\n...（只显示前 %d 行，用 max_rows 调整上限，最多 %d 行）", maxRows, sqlMaxRows)
	}
	return agent.Result{
		Content: text,
		Display: map[string]any{"columns": cols, "rows": count, "truncated": stopped},
	}, nil
}

// sqlGuard 做只读校验：单条语句 + 白名单开头 + 黑名单关键字。
func sqlGuard(q string) error {
	stripped := stripSQLComments(q)
	body := strings.TrimSpace(stripped)
	if body == "" {
		return fmt.Errorf("sql 不能为空")
	}

	// 单条语句：允许结尾一个分号，但中间不能再有分号。
	trimmed := strings.TrimRight(body, "; \t\r\n")
	if strings.Contains(trimmed, ";") {
		return fmt.Errorf("只允许执行单条 SQL 语句")
	}

	lower := strings.ToLower(trimmed)
	first := strings.Fields(lower)
	if len(first) == 0 {
		return fmt.Errorf("sql 不能为空")
	}
	switch first[0] {
	case "select", "show", "describe", "desc", "explain", "with":
	default:
		return fmt.Errorf("只允许只读查询（SELECT / SHOW / DESCRIBE / EXPLAIN / WITH），拒绝执行 %q", first[0])
	}
	for _, bad := range forbiddenSQL {
		if strings.Contains(lower, bad) {
			return fmt.Errorf("语句里含有被禁止的关键字 %q：本工具只允许只读查询", strings.TrimSpace(bad))
		}
	}
	return nil
}

// stripSQLComments 去掉 -- 与 /* */ 注释，避免用注释把关键字藏起来。
func stripSQLComments(q string) string {
	var b strings.Builder
	for i := 0; i < len(q); i++ {
		if i+1 < len(q) && q[i] == '-' && q[i+1] == '-' {
			for i < len(q) && q[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
			continue
		}
		if i+1 < len(q) && q[i] == '/' && q[i+1] == '*' {
			i += 2
			for i+1 < len(q) && !(q[i] == '*' && q[i+1] == '/') {
				i++
			}
			i++
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(q[i])
	}
	return b.String()
}

func sqlCell(v any) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return string(t)
	case time.Time:
		return t.Format("2006-01-02 15:04:05")
	default:
		return fmt.Sprint(t)
	}
}
