package mysql

import (
	"strings"
	"testing"
)

// repoMethodBody 返回源码中指定方法（以签名开头、以 "\n}\n" 结束）的方法体。
// 参数：t 为测试上下文，file 为源码文件名，signature 为方法签名前缀。
// 返回值：方法体字符串；签名不存在时失败。
func repoMethodBody(t *testing.T, file, signature string) string {
	t.Helper()
	source := readMySQLSource(t, file)
	start := strings.Index(source, signature)
	if start < 0 {
		t.Fatalf("%s 未找到 %s", file, signature)
	}
	end := strings.Index(source[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("%s 中 %s 方法未闭合", file, signature)
	}
	return source[start : start+end]
}

// TestFinancialEventReconcileIsPerContractVersion 验证补建与缺口计数都按
// (tenant, contract, contract_version) 判定：旧实现只看“合同已有任意事件”，
// 会让改版后的合同永远不补建（BIZ-XEVT2-05）。
// 参数：t 为测试上下文。
// 返回值：无。
func TestFinancialEventReconcileIsPerContractVersion(t *testing.T) {
	if !strings.Contains(financialEventVersionClause, "o.contract_id = c.id") || !strings.Contains(financialEventVersionClause, "o.contract_version = c.version") {
		t.Fatalf("财务事件幂等判定必须按合同版本：%s", financialEventVersionClause)
	}
	source := readMySQLSource(t, "payment_terms.go")
	// 旧的不带版本谓词不得残留：漏一处就会让该路径继续“永不补建”。
	if strings.Contains(source, "o.tenant_id = c.tenant_id AND o.contract_id = c.id)") {
		t.Fatal("仍存在不带 contract_version 的财务事件存在性判定")
	}
	for _, signature := range []string{
		"func (r *Repository) ReconcileFinancialEffectives(",
		"func (r *Repository) FinancialEffectiveBacklog(",
	} {
		body := repoMethodBody(t, "payment_terms.go", signature)
		if !strings.Contains(body, `"NOT EXISTS (SELECT 1 FROM con_financial_effective_outbox AS o WHERE " + financialEventVersionClause + ")"`) {
			t.Fatalf("%s 未使用按版本判定子句", signature)
		}
	}
	// “无计划=不发送”语义必须保留：补建仍要求已维护付款计划。
	reconcile := repoMethodBody(t, "payment_terms.go", "func (r *Repository) ReconcileFinancialEffectives(")
	if !strings.Contains(reconcile, "EXISTS (SELECT 1 FROM con_contract_payment_term AS t WHERE t.tenant_id = c.tenant_id AND t.contract_id = c.id)") {
		t.Fatal("补建丢失了“必须已维护付款计划”的前置条件")
	}
}

// TestApplyContractFinancialChangeIsSingleTransaction 以源码级守护验证受控变更
// 的写路径：单事务内 version+1、替换付款计划、写审计/生命周期并生成新版本事件；
// 审批前置（无在途审批 + 存在已通过实例）与币种锁定不得被移除。
// 参数：t 为测试上下文。
// 返回值：无。
func TestApplyContractFinancialChangeIsSingleTransaction(t *testing.T) {
	body := repoMethodBody(t, "payment_terms.go", "func (r *Repository) ApplyContractFinancialChange(")
	for _, required := range []struct{ name, fragment string }{
		{"单事务", "r.db.WithContext(ctx).Transaction("},
		{"行锁", `clause.Locking{Strength: "UPDATE"}`},
		{"版本自增", `"version": gorm.Expr("version + ?", 1)`},
		{"替换付款计划", "replaceContractPaymentTerms(tx, updated)"},
		{"财务事件入队", "enqueueFinancialEffective(tx, tenantID, row.ID)"},
		{"事件未入队失败关闭", "application.ErrFinancialChangeEventNotQueued"},
		{"生命周期留痕", "lifecycleEventRecord{"},
		{"审计同事务", "insertAuditOutbox(tx,"},
		{"审计动作", "CONTRACT_MANAGEMENT:FINANCIAL_TERMS_CHANGED"},
		{"在途审批门禁", "approval.StatusRunning"},
		{"已审批证据", "approval.StatusApproved"},
		{"普通来源门禁", "ordinaryContractRecord(row)"},
		{"状态门禁", "application.FinancialChangeStatusEligible"},
		{"币种锁定", "application.ErrFinancialChangeCurrencyLocked"},
	} {
		if !strings.Contains(body, required.fragment) {
			t.Errorf("受控财务变更缺少 %s：%s", required.name, required.fragment)
		}
	}
	// 审计载荷必须可解释金额与版本跃迁，且不携带客户隐私字段。
	for _, field := range []string{`"from_version"`, `"to_version"`, `"from_amount_minor"`, `"to_amount_minor"`, `"installment_count"`, `"approval_evidence"`} {
		if !strings.Contains(body, field) {
			t.Errorf("审计载荷缺少字段 %s", field)
		}
	}
	for _, forbidden := range []string{`"customer_name"`, `"customer_phone"`, `"customer_address"`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("审计载荷不得写入客户隐私字段 %s", forbidden)
		}
	}
}
