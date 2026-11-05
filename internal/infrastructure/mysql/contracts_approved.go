package mysql

import (
	"context"
	"strings"

	"github.com/j-s-te/contract-management/internal/domain/contract"
	"gorm.io/gorm"
)

// literalLikePattern 把用户关键字转成只匹配字面量的 LIKE 模式。
// 参数：keyword 为已去空白的搜索关键字。
// 返回值：首尾带通配符、内部 % _ \ 均已转义的模式串。
// 转义符固定为反斜杠（MySQL LIKE 的默认转义符），因此 SQL 里不写
// ESCAPE 子句，全部依赖默认口径；先转义反斜杠再转义通配符，
// 避免关键字里的 % 或 _ 变成通配符把整表扫成"匹配"。
// 所有接收用户关键字的 LIKE 入口都必须走本函数，不再各写一套拼接：
// 已批准合同引用（applyApprovedContractKeyword）、合同台账（contractLedgerFilters）、
// 签署台账（applySigningRecordFilters）、结算消费合同列表
// （ListIntegrationCompletedContractsScoped）、本地目录与授权可见目录
// （local_catalog*.go）。新增 LIKE 关键字入口时同样复用这里。
func literalLikePattern(keyword string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(keyword)
	return "%" + escaped + "%"
}

// applyApprovedContractKeyword 为已批准合同引用查询追加关键字过滤。
// 参数：query 为已带来源与范围谓词的查询，keyword 为原始关键字。
// 返回值：追加了绑定参数谓词的查询；空关键字原样返回。
func applyApprovedContractKeyword(query *gorm.DB, keyword string) *gorm.DB {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return query
	}
	pattern := literalLikePattern(keyword)
	return query.Where("(contract_number LIKE ? OR title LIKE ? OR customer_name LIKE ?)", pattern, pattern, pattern)
}

// ListApprovedContracts 读取租户内普通已批准合同的有界列表。
// 参数：ctx为上下文，tenantID为可信租户，limit为数量上限。
// 返回值：合同列表及付款计划，或数据库错误。
func (r *Repository) ListApprovedContracts(ctx context.Context, tenantID string, limit int) ([]contract.Contract, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var records []contractRecord
	if err := ordinaryContractScope(r.db.WithContext(ctx), "").Where("tenant_id = ? AND status IN ?", tenantID, contract.ApprovalPassedStatuses()).
		Omit("rendered_document", "template_values_json").Order("updated_at DESC").Limit(limit).Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]contract.Contract, 0, len(records))
	for _, record := range records {
		result = append(result, contractFromRecord(record))
	}
	return r.attachPaymentTerms(ctx, tenantID, result)
}

// ListApprovedContractReferences 按游标读取普通已批准合同引用，避免载入原件。
// 参数：ctx为上下文，tenantID为租户，keyword为可选关键字，afterID为游标，limit为页大小。
// 返回值：可见引用列表或数据库错误。
func (r *Repository) ListApprovedContractReferences(ctx context.Context, tenantID, keyword, afterID string, limit int) ([]contract.Contract, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	query := ordinaryContractScope(r.db.WithContext(ctx).Model(&contractRecord{}), "").
		Where("tenant_id = ? AND status IN ?", tenantID, contract.ApprovalPassedStatuses())
	query = applyApprovedContractKeyword(query, keyword)
	if afterID != "" {
		query = query.Where("id > ?", afterID)
	}
	var records []contractRecord
	if err := query.Select("id", "contract_number", "title", "customer_name", "status", "version", "counterparty_source", "category_source", "integration_policy").
		Order("id ASC").Limit(limit).Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]contract.Contract, 0, len(records))
	for _, record := range records {
		result = append(result, approvedContractReferenceFromRecord(record))
	}
	return result, nil
}

// ListApprovedContractsScoped 按可信责任范围读取普通已批准合同及付款计划。
// 参数：ctx为上下文，filter为租户及责任范围，limit为数量上限。
// 返回值：范围内合同列表或数据库错误。
func (r *Repository) ListApprovedContractsScoped(ctx context.Context, filter contract.ScopeFilter, limit int) ([]contract.Contract, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var records []contractRecord
	if err := ordinaryContractScope(applyContractScope(r.db.WithContext(ctx).Model(&contractRecord{}), filter), "").
		Where("status IN ?", contract.ApprovalPassedStatuses()).Omit("rendered_document", "template_values_json").
		Order("updated_at DESC").Limit(limit).Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]contract.Contract, 0, len(records))
	for _, record := range records {
		result = append(result, contractFromRecord(record))
	}
	return r.attachPaymentTerms(ctx, filter.TenantID, result)
}

// ListApprovedContractReferencesScoped 按责任范围和游标读取普通已批准合同引用。
// 参数：ctx为上下文，filter为可信范围，keyword为可选关键字，afterID为游标，limit为页大小。
// 返回值：可见合同引用或数据库错误。
func (r *Repository) ListApprovedContractReferencesScoped(ctx context.Context, filter contract.ScopeFilter, keyword, afterID string, limit int) ([]contract.Contract, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	query := ordinaryContractScope(applyContractScope(r.db.WithContext(ctx).Model(&contractRecord{}), filter), "").
		Where("status IN ?", contract.ApprovalPassedStatuses())
	query = applyApprovedContractKeyword(query, keyword)
	if afterID != "" {
		query = query.Where("id > ?", afterID)
	}
	var records []contractRecord
	if err := query.Select("id", "contract_number", "title", "customer_name", "status", "version", "counterparty_source", "category_source", "integration_policy").
		Order("id ASC").Limit(limit).Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]contract.Contract, 0, len(records))
	for _, record := range records {
		result = append(result, approvedContractReferenceFromRecord(record))
	}
	return result, nil
}

// approvedContractReferenceFromRecord 把引用列映射为合同摘要。
// 参数：record 为只选了引用列与展示列的行。
// 返回值：不含正文、价格与附件的合同摘要。
func approvedContractReferenceFromRecord(record contractRecord) contract.Contract {
	return contract.Contract{
		CounterpartySource: record.CounterpartySource, CategorySource: record.CategorySource, IntegrationPolicy: record.IntegrationPolicy,
		ID: record.ID, Number: valueOrEmpty(record.ContractNumber), Title: record.Title,
		CustomerName: valueOrEmpty(record.CustomerName), Status: contract.Status(record.Status), Version: record.Version,
	}
}
