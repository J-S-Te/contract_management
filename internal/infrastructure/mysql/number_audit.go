package mysql

import (
	"context"
	"errors"

	"github.com/j-s-te/contract-management/internal/migration"
)

// AuditContractNumberRegistry 以只读方式巡检合同编号与全系统登记表的一致性。
//
// 判定口径完全复用 internal/migration 的 AuditContractNumberRegistry：重复编号、
// 未登记、登记错配三类问题与迁移预检、cmd/contract-number-audit 保持同一套 SQL，
// 避免周期巡检和运维命令行给出互相矛盾的结论。仓储层只负责把 gorm 连接还原成
// 标准库连接池，不写库、不建事务、不写审计 outbox。
//
// 参数：ctx 为上下文；调用方应使用带超时的活动上下文。
// 返回值：巡检报告；取不到底层连接池或巡检失败时返回错误，由 Temporal 按活动
// 重试策略重试。
func (r *Repository) AuditContractNumberRegistry(ctx context.Context) (migration.AuditReport, error) {
	if r == nil || r.db == nil {
		return migration.AuditReport{}, errors.New("编号巡检不可用：仓储未初始化")
	}
	database, err := r.db.WithContext(ctx).DB()
	if err != nil {
		return migration.AuditReport{}, err
	}
	return migration.AuditContractNumberRegistry(ctx, database)
}
