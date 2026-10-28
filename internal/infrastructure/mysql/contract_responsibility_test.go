package mysql

import (
	"encoding/json"
	"gorm.io/gorm/schema"
	"sync"
	"testing"
	"time"
)

// TestResponsibilityPayloadUsesOwnerAndIndependentVersion 防止创建人或财务版本替代真实责任。
// 参数：t 为测试上下文。
// 返回值：无。
func TestResponsibilityPayloadUsesOwnerAndIndependentVersion(t *testing.T) {
	row := contractRecord{ID: "contract", TenantID: "tenant", OwnerUserID: "successor", CreatedBy: "creator", Version: 99, Status: "active"}
	raw, err := newResponsibilityPayload("event", row, 2, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["owner_user_id"] != "successor" || got["source_version"] != float64(2) || got["active"] != true {
		t.Fatalf("责任载荷=%s", raw)
	}
	for _, status := range []string{"terminated", "archived"} {
		row.Status = status
		raw, _ = newResponsibilityPayload("event", row, 3, time.Now())
		_ = json.Unmarshal(raw, &got)
		if got["active"] != false {
			t.Fatalf("终态未撤销：%s", raw)
		}
	}
}

// TestResponsibilityOutboxPersistsFrozenPayload 验证GORM能映射责任队列的必需持久字段。
// 参数：t 为测试上下文。
// 返回值：无。
func TestResponsibilityOutboxPersistsFrozenPayload(t *testing.T) {
	parsed, err := schema.Parse(&contractResponsibilityOutbox{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"id", "payload_json", "contract_version", "locked_at", "delivery_status"} {
		if parsed.FieldsByDBName[name] == nil {
			t.Fatalf("责任Outbox字段%s未持久映射", name)
		}
	}
}

// TestFinancialPayloadCarriesAuthoritativeOwnerAndProject 验证财务初始事件携带已有真实责任引用。
// 参数：t 为测试上下文。
// 返回值：无。
func TestFinancialPayloadCarriesAuthoritativeOwnerAndProject(t *testing.T) {
	project := "real-project"
	row := contractRecord{ID: "contract", TenantID: "tenant", OwnerUserID: "actual-owner", CreatedBy: "creator", Version: 5, ProjectID: &project}
	raw, err := newFinancialEffectivePayload("event", &row, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var event financialEffectivePayload
	if err = json.Unmarshal(raw, &event); err != nil {
		t.Fatal(err)
	}
	if event.Contract.OwnerUserID != "actual-owner" || event.Contract.ProjectID != "real-project" {
		t.Fatalf("财务事件缺失真实责任引用：%s", raw)
	}
}
