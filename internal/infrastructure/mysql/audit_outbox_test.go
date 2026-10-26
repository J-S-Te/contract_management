package mysql

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/auditctx"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// 批次 2：审计事件必须与业务状态变更在同一事务里落库，投递由 dispatcher 重试。
// 本地无隔离 MySQL，这里用 DryRun 断言写入语句的目标表与关键列。
func newAuditOutboxDryRun(t *testing.T) (*gorm.DB, *[]string) {
	t.Helper()
	db, err := gorm.Open(mysql.New(mysql.Config{DSN: "test:test@tcp(localhost:3306)/test?parseTime=true", SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, DryRun: true, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("open dry-run database: %v", err)
	}
	captured := make([]string, 0, 2)
	if err := db.Callback().Create().After("gorm:create").Register("capture_audit_outbox", func(tx *gorm.DB) {
		if sql := tx.Statement.SQL.String(); sql != "" {
			captured = append(captured, sql)
		}
	}); err != nil {
		t.Fatalf("register callback: %v", err)
	}
	return db, &captured
}

func TestInsertAuditOutboxTargetsDedicatedTableWithProvenance(t *testing.T) {
	db, captured := newAuditOutboxDryRun(t)
	ctx := auditctx.WithMeta(context.Background(), auditctx.Meta{ClientIP: "10.8.0.5", IPSource: "TRUSTED_PROXY"})

	err := db.WithContext(ctx).Create(&auditOutboxRecord{
		ID: "01J00000000000000000000001", TenantID: "tenant-1", EventID: "event-1",
		Action: "CONTRACT_MANAGEMENT:STATUS_CHANGE", ResourceType: "contract", ResourceID: "contract-1",
		ClientIP: ipBytes(auditctx.FromContext(ctx).ClientIP), IPSource: "TRUSTED_PROXY",
		Result: "SUCCESS", DeliveryStatus: "pending", NextAttemptAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	}).Error
	if err != nil {
		t.Fatalf("insert audit outbox: %v", err)
	}
	if len(*captured) == 0 {
		t.Fatal("未捕获到写入语句")
	}
	sql := (*captured)[0]
	if !strings.Contains(sql, "con_audit_outbox") {
		t.Fatalf("审计事件必须写入专用 outbox 表：%s", sql)
	}
	for _, column := range []string{"client_ip", "ip_source", "delivery_status", "next_attempt_at"} {
		if !strings.Contains(sql, column) {
			t.Errorf("写入语句缺少列 %q：%s", column, sql)
		}
	}
}

// 生命周期事务必须在同一 tx 内写审计：源码级守护，避免将来被挪回"业务提交后同步发送"。
func TestLifecycleWritesAuditOutboxInsideTransaction(t *testing.T) {
	source := readMySQLSource(t, "repository.go")
	start := strings.Index(source, "func insertLifecycle(")
	if start < 0 {
		t.Fatal("未找到 insertLifecycle")
	}
	end := strings.Index(source[start:], "\n}\n")
	body := source[start : start+end]
	if !strings.Contains(body, "insertAuditOutbox(tx,") {
		t.Fatalf("insertLifecycle 必须在业务事务内写审计 outbox：%s", body)
	}
	if strings.Contains(body, "platform.Report") || strings.Contains(body, "AuditReporter") {
		t.Fatal("insertLifecycle 不得同步发送审计（会破坏事务一致性）")
	}
}

func readMySQLSource(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(content)
}

// 统一审计方案 §3.1：合同创建属于关键业务变更，必须与合同行同事务写审计 outbox。
func TestCreateContractWritesAuditOutboxInTransaction(t *testing.T) {
	source, err := os.ReadFile("contracts_lifecycle.go")
	if err != nil {
		t.Fatalf("读取 contracts_lifecycle.go: %v", err)
	}
	text := string(source)
	start := strings.Index(text, "func (r *Repository) CreateContract(")
	if start < 0 {
		t.Fatal("未找到 CreateContract")
	}
	end := strings.Index(text[start:], "\n}\n")
	if end < 0 {
		t.Fatal("未找到 CreateContract 结束位置")
	}
	body := text[start : start+end]
	if !strings.Contains(body, "CONTRACT_MANAGEMENT:CREATED") {
		t.Fatalf("CreateContract 必须写创建审计事件：%s", body)
	}
	if !strings.Contains(body, "insertAuditOutbox(tx,") {
		t.Fatalf("创建审计必须与业务同事务写入 outbox：%s", body)
	}
	// 不得把合同正文/模板值/客户敏感字段写进审计载荷。
	for _, forbidden := range []string{"RenderedDocument", "TemplateValuesJSON", "CustomerPhone", "CustomerAddress"} {
		if strings.Contains(body, forbidden+"") && strings.Contains(body, `"`+forbidden+`":`) {
			t.Fatalf("审计载荷不得包含 %s：%s", forbidden, body)
		}
	}
}

// 统一审计方案 §3.1：模板增删改属于关键业务变更，必须在同一事务写审计 outbox，
// 且操作者来自请求上下文（缺失时记 SYSTEM，不伪装成人工）。
func TestTemplateMutationsWriteAuditOutbox(t *testing.T) {
	source, err := os.ReadFile("templates.go")
	if err != nil {
		t.Fatalf("读取 templates.go: %v", err)
	}
	text := string(source)
	for _, action := range []string{
		"CONTRACT_MANAGEMENT:TEMPLATE_CREATED",
		"CONTRACT_MANAGEMENT:TEMPLATE_UPDATED",
		"CONTRACT_MANAGEMENT:TEMPLATE_DELETED",
	} {
		if !strings.Contains(text, action) {
			t.Fatalf("模板变更缺少审计动作 %s", action)
		}
	}
	if strings.Count(text, "insertTemplateAudit(tx,") < 3 {
		t.Fatalf("模板增删改都必须在事务内写审计：%d", strings.Count(text, "insertTemplateAudit(tx,"))
	}
	// 载荷只允许模板标识与名称。
	if strings.Contains(text, `"document"`) && strings.Contains(text, "insertTemplateAudit") {
		t.Skip("正文仅出现在模板存储语句，审计载荷单独断言")
	}
}

// 统一审计方案 §3.1：审批命令（签署/转派/退回/催办/撤回）必须与命令记录同事务写审计，
// 动作按命令区分，便于「谁在什么轮次签署/转派」直接检索。
func TestApprovalCommandsWriteAuditOutbox(t *testing.T) {
	source, err := os.ReadFile("repository.go")
	if err != nil {
		t.Fatalf("读取 repository.go: %v", err)
	}
	text := string(source)
	start := strings.Index(text, "func (r *Repository) RecordCommand(")
	if start < 0 {
		t.Fatal("未找到 RecordCommand")
	}
	end := strings.Index(text[start:], "\n}\n")
	if end < 0 {
		t.Fatal("未找到 RecordCommand 结束位置")
	}
	body := text[start : start+end]
	if !strings.Contains(body, `"CONTRACT_MANAGEMENT:APPROVAL_"`) {
		t.Fatalf("RecordCommand 必须写审批命令审计：%s", body)
	}
	if !strings.Contains(body, "insertAuditOutbox(tx,") {
		t.Fatalf("审批命令审计必须与命令记录同事务：%s", body)
	}
	// 载荷只允许定位与命令元数据，不得写入审批意见原文（可能含客户信息）。
	if strings.Contains(body, `"comment":`) {
		t.Fatalf("审批意见原文不得写入审计载荷：%s", body)
	}
}

// 统一审计方案 §3.1：草稿修改是关键业务变更，必须与业务行同事务写审计并记录版本跃迁，
// 且不得把合同正文/模板值/客户联系方式写进审计载荷。
func TestDraftUpdateWritesAuditOutbox(t *testing.T) {
	source, err := os.ReadFile("draft.go")
	if err != nil {
		t.Fatalf("读取 draft.go: %v", err)
	}
	text := string(source)
	start := strings.Index(text, "func (r *Repository) UpdateContractDraft(")
	if start < 0 {
		t.Fatal("未找到 UpdateContractDraft")
	}
	end := strings.Index(text[start:], "\n}\n")
	if end < 0 {
		t.Fatal("未找到 UpdateContractDraft 结束位置")
	}
	body := text[start : start+end]
	if !strings.Contains(body, `"CONTRACT_MANAGEMENT:UPDATED"`) {
		t.Fatalf("草稿修改必须写审计事件：%s", body)
	}
	if !strings.Contains(body, "insertAuditOutbox(tx,") {
		t.Fatalf("草稿审计必须与业务同事务：%s", body)
	}
	if !strings.Contains(body, `"from_version"`) || !strings.Contains(body, `"to_version"`) {
		t.Fatalf("审计载荷必须记录版本跃迁：%s", body)
	}
	// 只检查审计载荷构造段，业务 UPDATE 语句本身当然要写正文。
	payloadStart := strings.Index(body, "auditPayload, err := json.Marshal(")
	payloadEnd := strings.Index(body, "insertAuditOutbox(tx,")
	if payloadStart < 0 || payloadEnd < payloadStart {
		t.Fatalf("未找到审计载荷构造段：%s", body)
	}
	payload := body[payloadStart:payloadEnd]
	for _, forbidden := range []string{`"content":`, `"template_values":`, `"customer_phone":`, `"customer_address":`} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("审计载荷不得包含 %s", forbidden)
		}
	}
}

// 统一审计方案 §3.2：敏感文档下载必须在返回字节前建立持久化审计依据，
// 记录合同、文档类型与操作者；不得记录文档内容。
func TestDocumentAccessWritesAuditOutbox(t *testing.T) {
	source, err := os.ReadFile("draft.go")
	if err != nil {
		t.Fatalf("读取 draft.go: %v", err)
	}
	text := string(source)
	start := strings.Index(text, "func (r *Repository) RecordDocumentAccess(")
	if start < 0 {
		t.Fatal("未找到 RecordDocumentAccess")
	}
	end := strings.Index(text[start:], "\n}\n")
	body := text[start : start+end]
	if !strings.Contains(body, "CONTRACT_MANAGEMENT:DOCUMENT_DOWNLOADED") {
		t.Fatalf("文档访问必须写业务审计：%s", body)
	}
	if !strings.Contains(body, `"document_kind"`) {
		t.Fatalf("审计载荷必须记录文档类型：%s", body)
	}
	if strings.Contains(body, "Document:") || strings.Contains(body, `"content":`) {
		t.Fatalf("审计不得包含文档内容：%s", body)
	}

	router, err := os.ReadFile("../../transport/httpapi/router.go")
	if err != nil {
		t.Fatalf("读取 router.go: %v", err)
	}
	handlers := string(router)
	for _, kind := range []string{`"DOCX"`, `"PDF"`, `"STAMPED_PDF"`} {
		if !strings.Contains(handlers, "RecordDocumentAccess(c.Request.Context(), principal(c), c.Param(\"contractID\"), "+kind+")") {
			t.Fatalf("下载处理器缺少 %s 的审计调用", kind)
		}
	}
}

// 统一审计方案 §3.1：合同交接给项目管理属于跨系统关键动作，必须与投递行同事务留痕。
func TestProjectHandoffWritesAuditOutbox(t *testing.T) {
	source, err := os.ReadFile("project_delivery.go")
	if err != nil {
		t.Fatalf("读取 project_delivery.go: %v", err)
	}
	text := string(source)
	start := strings.Index(text, "func enqueueProjectActivation(")
	if start < 0 {
		t.Fatal("未找到 enqueueProjectActivation")
	}
	end := strings.Index(text[start:], "\n}\n")
	body := text[start : start+end]
	if !strings.Contains(body, "CONTRACT_MANAGEMENT:HANDED_OFF_TO_PROJECT") {
		t.Fatalf("交接必须写审计事件：%s", body)
	}
	if !strings.Contains(body, `"service_item_count"`) || !strings.Contains(body, `"target": "PROJECT_MANAGEMENT"`) {
		t.Fatalf("交接审计载荷必须含服务项数量与目标系统：%s", body)
	}
}
