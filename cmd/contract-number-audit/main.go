// Command contract-number-audit 巡检合同编号与全系统登记表的一致性。
//
// 它是运维入口：读 MYSQL_DSN（与 cmd/migrate 同一环境变量风格）建立只读会话，
// 再调用 internal/migration 的巡检逻辑，只负责装配与中文输出。三类问题分别是
// 重复编号、未登记、登记错配；有任何问题以退出码 1 结束，全干净以退出码 0 结束。
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	driver "github.com/go-sql-driver/mysql"

	"github.com/j-s-te/contract-management/internal/migration"
)

// auditReportLineLimit 是巡检输出每一类问题最多打印的明细条数。
const auditReportLineLimit = 20

func main() {
	os.Exit(run(os.Stdout, os.Stderr, os.Getenv("MYSQL_DSN")))
}

// run 是命令主体，便于测试装配与输出分支。
//
// 参数：stdout、stderr 为输出目标，dsn 为 MYSQL_DSN 原值。
// 返回值：进程退出码，0 表示一致，1 表示发现问题或执行失败。
func run(stdout, stderr io.Writer, dsn string) int {
	if dsn == "" {
		fmt.Fprintln(stderr, "MYSQL_DSN 未配置：巡检需要与 cmd/migrate 相同的数据库连接串")
		return 1
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		fmt.Fprintln(stderr, "MYSQL_DSN 解析失败：请检查连接串格式")
		return 1
	}
	if cfg.DBName == "" {
		fmt.Fprintln(stderr, "MYSQL_DSN 未指定数据库名：巡检必须指向合同服务库")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	database, err := openReadOnly(ctx, cfg.FormatDSN())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer database.Close()
	report, err := migration.AuditContractNumberRegistry(ctx, database)
	if err != nil {
		fmt.Fprintf(stderr, "巡检未能完成：%v\n", err)
		return 1
	}
	return emitReport(stdout, report)
}

// emitReport 输出巡检报告并返回约定退出码，便于脱离数据库验证退出码约定。
//
// 参数：writer 为输出目标，report 为巡检结果。
// 返回值：0 表示一致，1 表示发现问题。
func emitReport(writer io.Writer, report migration.AuditReport) int {
	writeReport(writer, report)
	if !report.Clean() {
		return 1
	}
	return 0
}

// openReadOnly 建立只读会话，保证巡检不会误改业务库。
//
// 连接池限制为单连接，使 SET SESSION TRANSACTION READ ONLY 稳定作用于后续全部查询；
// 只读会话下任何隐式写事务都会被 MySQL 拒绝。
//
// 参数：ctx 为上下文，dsn 为归一后的连接串。
// 返回值：只读连接池或中文错误。
func openReadOnly(ctx context.Context, dsn string) (*sql.DB, error) {
	database, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("巡检无法建立数据库连接，请联系技术排查：%w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	if err := database.PingContext(ctx); err != nil {
		database.Close()
		return nil, fmt.Errorf("巡检无法连接数据库，请检查网络与账号权限：%w", err)
	}
	if _, err := database.ExecContext(ctx, `SET SESSION TRANSACTION READ ONLY`); err != nil {
		database.Close()
		return nil, fmt.Errorf("巡检无法开启只读会话，已中止以免影响业务库：%w", err)
	}
	return database, nil
}

// writeReport 输出巡检报告。
//
// 参数：writer 为输出目标，report 为巡检结果。
// 返回值：无。
func writeReport(writer io.Writer, report migration.AuditReport) {
	fmt.Fprintln(writer, "== 合同编号巡检 ==")
	writeDuplicates(writer, report.Duplicates)
	fmt.Fprintf(writer, "\n[未登记] 共 %d 条\n", report.UnregisteredTotal)
	for index, item := range report.Unregistered {
		if index >= auditReportLineLimit {
			break
		}
		fmt.Fprintf(writer, "  %s\n", item.String())
	}
	writeHidden(writer, report.UnregisteredTotal, len(report.Unregistered))
	fmt.Fprintf(writer, "\n[登记错配] 共 %d 条\n", report.MismatchedTotal)
	for index, item := range report.Mismatched {
		if index >= auditReportLineLimit {
			break
		}
		fmt.Fprintf(writer, "  %s\n", item.String())
	}
	writeHidden(writer, report.MismatchedTotal, len(report.Mismatched))
	fmt.Fprintln(writer)
	if report.Clean() {
		fmt.Fprintln(writer, "合同编号与登记表一致")
		return
	}
	fmt.Fprintf(writer, "结论：共发现 %d 处问题（重复编号 %d 组、未登记 %d 条、登记错配 %d 条），"+
		"请按上述清单清理或受控更正后重新巡检。\n",
		report.Total(), len(report.Duplicates), report.UnregisteredTotal, report.MismatchedTotal)
}

// writeDuplicates 输出重复编号段。
//
// 参数：writer 为输出目标，groups 为全部重复组。
// 返回值：无。
func writeDuplicates(writer io.Writer, groups []migration.DuplicateContractNumber) {
	fmt.Fprintf(writer, "[重复编号] 共 %d 组\n", len(groups))
	shown := groups
	if len(shown) > auditReportLineLimit {
		shown = shown[:auditReportLineLimit]
	}
	for _, group := range shown {
		fmt.Fprintf(writer, "  %s\n", group.String())
	}
	writeHidden(writer, len(groups), len(shown))
}

// writeHidden 在明细被截断时说明剩余条数。
//
// 参数：writer 为输出目标，total 为问题总数，printed 为已打印条数。
// 返回值：无。
func writeHidden(writer io.Writer, total, printed int) {
	if printed > auditReportLineLimit {
		printed = auditReportLineLimit
	}
	if rest := total - printed; rest > 0 {
		fmt.Fprintf(writer, "  ……其余 %d 条未显示，请按编号逐条排查\n", rest)
	}
}
