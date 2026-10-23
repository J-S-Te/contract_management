package mysql

import (
	"net"
	"strings"
	"time"

	"gorm.io/gorm"
)

// auditOutboxRecord 是事务内写下的业务审计事件（批次 2）。
//
// 业务状态变更与审计事件在同一事务提交；投递由后台 dispatcher 完成，
// 平台不可用时事件留在本地重试，不再用"同步发送失败返 503"冒充一致性。
type auditOutboxRecord struct {
	ID             string     `gorm:"column:id;primaryKey"`
	TenantID       string     `gorm:"column:tenant_id"`
	EventID        string     `gorm:"column:event_id"`
	ActorID        *string    `gorm:"column:actor_id"`
	Action         string     `gorm:"column:action"`
	ResourceType   string     `gorm:"column:resource_type"`
	ResourceID     string     `gorm:"column:resource_id"`
	RequestID      *string    `gorm:"column:request_id"`
	LoginIP        []byte     `gorm:"column:login_ip"`
	ClientIP       []byte     `gorm:"column:client_ip"`
	IPSource       string     `gorm:"column:ip_source"`
	Result         string     `gorm:"column:result"`
	PayloadJSON    []byte     `gorm:"column:payload_json"`
	DeliveryStatus string     `gorm:"column:delivery_status"`
	Attempts       uint       `gorm:"column:attempts"`
	NextAttemptAt  time.Time  `gorm:"column:next_attempt_at"`
	LockedAt       *time.Time `gorm:"column:locked_at"`
	DeliveredAt    *time.Time `gorm:"column:delivered_at"`
	LastError      *string    `gorm:"column:last_error"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
}

func (auditOutboxRecord) TableName() string { return "con_audit_outbox" }

// AuditOutboxEntry 描述一条待投递的业务审计事件。
type AuditOutboxEntry struct {
	ActorID, Action, ResourceType, ResourceID, RequestID string
	LoginIP, ClientIP, IPSource                          string
	Result                                               string
	Payload                                              []byte
}

// insertAuditOutbox 在业务事务内写入审计事件；只能由已开启的事务调用。
func insertAuditOutbox(tx *gorm.DB, tenantID, eventID string, entry AuditOutboxEntry) error {
	now := time.Now().UTC()
	ipSource := entry.IPSource
	if ipSource == "" {
		ipSource = "UNKNOWN"
	}
	result := entry.Result
	if result == "" {
		result = "SUCCESS"
	}
	return tx.Create(&auditOutboxRecord{
		ID: newID(), TenantID: tenantID, EventID: eventID,
		ActorID: stringPtr(entry.ActorID), Action: entry.Action,
		ResourceType: entry.ResourceType, ResourceID: entry.ResourceID,
		RequestID: stringPtr(entry.RequestID),
		LoginIP:   ipBytes(entry.LoginIP), ClientIP: ipBytes(entry.ClientIP), IPSource: ipSource,
		Result: result, PayloadJSON: entry.Payload,
		DeliveryStatus: "pending", NextAttemptAt: now, CreatedAt: now,
	}).Error
}

// ipBytes 把规范化后的 IP 字面量存成 VARBINARY(16)；非法值写 NULL，避免脏数据。
func ipBytes(value string) []byte {
	parsed := net.ParseIP(strings.TrimSpace(value))
	if parsed == nil {
		return nil
	}
	if ipv4 := parsed.To4(); ipv4 != nil {
		return append([]byte(nil), ipv4...)
	}
	return append([]byte(nil), parsed.To16()...)
}
