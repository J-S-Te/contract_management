package mysql

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/businessclock"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"gorm.io/gorm"
)

const signingLedgerOrder = "CASE WHEN s.status IS NULL OR s.status <> 'completed' THEN 0 ELSE 1 END, c.updated_at DESC, c.id DESC"

// signingLedgerQuery 从合同左连接签署行，按合同数据范围过滤，避免先截取最近
// 200 份合同而漏掉较早的未完成签署。
func signingLedgerQuery(db *gorm.DB, filter contract.ScopeFilter) *gorm.DB {
	scope := applyContractScope(db.Model(&contractRecord{}), filter).Select("id")
	return db.Table("con_contract AS c").
		Joins("LEFT JOIN con_contract_signing AS s ON s.contract_id = c.id AND s.tenant_id = c.tenant_id").
		Where("c.id IN (?)", scope).
		Where("c.status IN ?", contract.ApprovalPassedStatuses())
}

func applySigningRecordFilters(query *gorm.DB, filter contract.SigningRecordQuery, today time.Time) *gorm.DB {
	if keyword := strings.TrimSpace(filter.Keyword); keyword != "" {
		// 关键字模式与合同台账/已批准引用同一口径：% _ \ 按字面量转义，
		// 否则 "100%" 会退化成通配符，把签署台账的 total 和分页一起放大。
		pattern := literalLikePattern(keyword)
		query = query.Where("(c.contract_number LIKE ? OR c.title LIKE ? OR c.customer_name LIKE ? OR s.courier_number LIKE ?)", pattern, pattern, pattern, pattern)
	}
	switch strings.TrimSpace(filter.Status) {
	case "":
	case "pending_shipment":
		query = query.Where("(s.status IS NULL OR s.status = ?) AND (c.end_date IS NULL OR c.end_date >= ?)", contract.SigningPendingShipment, today)
	case "in_return", "pending_review":
		// 页面把已过期的未完成记录统一显示为“已失效”，状态筛选必须与显示一致。
		query = query.Where("s.status = ? AND (c.end_date IS NULL OR c.end_date >= ?)", strings.TrimSpace(filter.Status), today)
	case "completed":
		query = query.Where("s.status = ?", contract.SigningCompleted)
	case "expired":
		query = query.Where("(s.status IS NULL OR s.status <> ?) AND c.end_date IS NOT NULL AND c.end_date < ?", contract.SigningCompleted, today)
	default:
		query = query.Where("1 = 0")
	}
	if method := strings.TrimSpace(filter.Method); method != "" {
		query = query.Where("COALESCE(s.method, 'paper') = ?", method)
	}
	return query
}

// ListSigningRecordsScoped 在服务端完成状态/关键字/方式筛选与分页，未完成签署优先，
// 并返回全量统计，避免页面只看到最近一页却把它当成完整台账。

func (r *Repository) ListSigningRecordsScoped(ctx context.Context, filter contract.ScopeFilter, query contract.SigningRecordQuery) (contract.SigningRecordPage, error) {
	if query.Limit <= 0 || query.Limit > 200 {
		query.Limit = 50
	}
	if query.Offset < 0 {
		query.Offset = 0
	}
	// 台账的“今天”是业务日期（北京日历日）。库里的 DATE 列（end_date）按 UTC 墙钟保存，
	// 驱动默认 loc=UTC 又会把参数换算成 UTC 再格式化，因此这里必须用 CalendarDay 的
	// 「同日 00:00 UTC 墙钟」形式；直接传北京市区 00:00 会变成前一天 16:00，整条比较错开 8 小时。
	today := businessclock.CalendarDay(businessclock.Now())
	db := r.db.WithContext(ctx)

	countQuery := applySigningRecordFilters(signingLedgerQuery(db, filter), query, today)
	var total int64
	if err := countQuery.Distinct("c.id").Count(&total).Error; err != nil {
		return contract.SigningRecordPage{}, err
	}
	ids := make([]string, 0, query.Limit)
	if int64(query.Offset) < total {
		pageQuery := applySigningRecordFilters(signingLedgerQuery(db, filter), query, today)
		if err := pageQuery.Select("c.id").Order(signingLedgerOrder).Offset(query.Offset).Limit(query.Limit).Pluck("c.id", &ids).Error; err != nil {
			return contract.SigningRecordPage{}, err
		}
	}
	contracts := make([]contract.Contract, 0, len(ids))
	if len(ids) > 0 {
		var records []contractRecord
		if err := db.Where("tenant_id = ? AND id IN ?", filter.TenantID, ids).Omit("rendered_document", "template_values_json").Find(&records).Error; err != nil {
			return contract.SigningRecordPage{}, err
		}
		byID := make(map[string]contractRecord, len(records))
		for _, record := range records {
			byID[record.ID] = record
		}
		for _, id := range ids {
			if record, ok := byID[id]; ok {
				contracts = append(contracts, contractFromRecord(record))
			}
		}
		var err error
		contracts, err = r.attachPaymentTerms(ctx, filter.TenantID, contracts)
		if err != nil {
			return contract.SigningRecordPage{}, err
		}
	}
	items, err := r.signingRecordsForContracts(ctx, filter.TenantID, contracts)
	if err != nil {
		return contract.SigningRecordPage{}, err
	}
	stats, err := r.signingStatsScoped(ctx, filter, today)
	if err != nil {
		return contract.SigningRecordPage{}, err
	}
	return contract.SigningRecordPage{
		Items: items, Total: total, Offset: query.Offset, Limit: query.Limit,
		HasMore: int64(query.Offset+len(ids)) < total, Stats: stats,
	}, nil
}

// signingStatsScoped 汇总签署台账统计。
// 参数：today 为数据库口径的业务日期（见 ListSigningRecordsScoped 的 CalendarDay 说明），
// 只用于 DATE 列比较；“本月已确认”与 confirmed_at 这类 DATETIME 绝对时刻比较时，
// 月界必须换成北京时间月初对应的真实瞬间（月界.UTC()），不能用 today 推。
func (r *Repository) signingStatsScoped(ctx context.Context, filter contract.ScopeFilter, today time.Time) (contract.SigningStats, error) {
	monthStart := businessclock.MonthStart()
	nextMonth := monthStart.AddDate(0, 1, 0)
	var stats contract.SigningStats
	err := signingLedgerQuery(r.db.WithContext(ctx), filter).Select(`COUNT(*) AS total,
		COALESCE(SUM(CASE WHEN s.status = 'completed' THEN 1 ELSE 0 END), 0) AS completed,
		COALESCE(SUM(CASE WHEN COALESCE(s.status, 'pending_shipment') IN ('pending_shipment', 'in_return', 'pending_review')
			AND (c.end_date IS NULL OR c.end_date >= ?) THEN 1 ELSE 0 END), 0) AS processing,
		COALESCE(SUM(CASE WHEN s.status = 'completed' AND s.confirmed_at >= ? AND s.confirmed_at < ? THEN 1 ELSE 0 END), 0) AS monthly,
		COALESCE(SUM(CASE WHEN (s.status IS NULL OR s.status <> 'completed') AND c.end_date IS NOT NULL AND c.end_date < ? THEN 1 ELSE 0 END), 0) AS expired`,
		today, monthStart.UTC(), nextMonth.UTC(), today).Scan(&stats).Error
	if err != nil {
		return contract.SigningStats{}, err
	}
	return stats, nil
}

func (r *Repository) signingRecordsForContracts(ctx context.Context, tenantID string, contracts []contract.Contract) ([]contract.SigningRecord, error) {
	ids := make([]string, 0, len(contracts))
	for _, item := range contracts {
		ids = append(ids, item.ID)
	}
	var signingRows []signingRecord
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND contract_id IN ?", tenantID, ids).Find(&signingRows).Error; err != nil {
		return nil, err
	}
	var documents []stampedDocumentRecord
	if err := r.db.WithContext(ctx).Select("contract_id", "version", "original_filename", "content_sha256", "uploaded_at").
		Where("tenant_id = ? AND contract_id IN ?", tenantID, ids).Order("contract_id ASC, version ASC").Find(&documents).Error; err != nil {
		return nil, err
	}
	byID := make(map[string]signingRecord, len(signingRows))
	for _, row := range signingRows {
		byID[row.ContractID] = row
	}
	docByID := make(map[string]stampedDocumentRecord, len(documents))
	for _, row := range documents {
		docByID[row.ContractID] = row
	}
	result := make([]contract.SigningRecord, 0, len(contracts))
	for _, item := range contracts {
		result = append(result, signingFromRecords(item, byID[item.ID], docByID[item.ID]))
	}
	return result, nil
}

func (r *Repository) GetSigningRecord(ctx context.Context, tenantID, contractID string) (contract.SigningRecord, error) {
	item, err := r.GetContract(ctx, tenantID, contractID)
	if err != nil {
		return contract.SigningRecord{}, err
	}
	var signing signingRecord
	err = r.db.WithContext(ctx).Where("tenant_id = ? AND contract_id = ?", tenantID, contractID).Take(&signing).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return contract.SigningRecord{}, err
	}
	var document stampedDocumentRecord
	err = r.db.WithContext(ctx).Select("contract_id", "version", "original_filename", "content_sha256", "uploaded_at").
		Where("tenant_id = ? AND contract_id = ?", tenantID, contractID).Order("version DESC").Take(&document).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return contract.SigningRecord{}, err
	}
	// 存量明文手机号懒回填（SEC-D7）：读取时顺手加密，掩码不依赖回填成功。
	r.repairSigningPhonePlaintext(ctx, tenantID, contractID, signing)
	return signingFromRecords(item, signing, document), nil
}

func (r *Repository) GetSigningRecordScoped(ctx context.Context, filter contract.ScopeFilter, contractID string) (contract.SigningRecord, error) {
	item, err := r.GetContractScoped(ctx, filter, contractID)
	if err != nil {
		return contract.SigningRecord{}, err
	}
	var signing signingRecord
	err = r.db.WithContext(ctx).Where("tenant_id = ? AND contract_id = ?", filter.TenantID, contractID).Take(&signing).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return contract.SigningRecord{}, err
	}
	var document stampedDocumentRecord
	err = r.db.WithContext(ctx).Select("contract_id", "version", "original_filename", "content_sha256", "uploaded_at").
		Where("tenant_id = ? AND contract_id = ?", filter.TenantID, contractID).Order("version DESC").Take(&document).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return contract.SigningRecord{}, err
	}
	// 存量明文手机号懒回填（SEC-D7）：读取时顺手加密，掩码不依赖回填成功。
	r.repairSigningPhonePlaintext(ctx, filter.TenantID, contractID, signing)
	return signingFromRecords(item, signing, document), nil
}

// maskStoredSigningPhone 是读路径的掩码投影（SEC-D7）：API 只返回 138****5678
// 形式的掩码号码，完整号码不出读路径。
//   - 密文：解密后掩码；解密失败（密钥缺失/篡改）返回固定占位，绝不回传密文；
//   - 存量明文（加密上线前写入）：直接掩码，回填由 repairSigningPhonePlaintext 负责。
