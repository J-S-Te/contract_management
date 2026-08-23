// Package notification 把合同通知 outbox 投递到基础平台站内信 ingestion 端点。
package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Delivery 是一条待投递的合同通知快照，字段来自 con_notification_outbox。
type Delivery struct {
	ID                string
	TenantID          string
	RecipientKey      string
	RecipientUserID   *string
	RecipientRoleCode *string
	NotificationType  string
	Title             string
	Content           string
	ContractID        *string
	ApprovalID        *string
	DedupeKey         string
	Attempts          uint
	CreatedAt         time.Time
}

// Store 是通知 outbox 的领取/完成/失败仓储契约，由 mysql 仓储实现。
type Store interface {
	ClaimNotificationDelivery(context.Context) (Delivery, bool, error)
	MarkNotificationDelivered(context.Context, string) error
	MarkNotificationFailed(context.Context, string, string, uint, bool) error
}

// Dispatcher 周期性领取 pending 通知并投递到平台 /notifications/events。
type Dispatcher struct {
	Store       Store
	BaseURL     string
	MaxAttempts uint
	Poll        time.Duration
	Client      *http.Client
	Logger      *slog.Logger
	TokenSource func(context.Context) (string, error)
	// ResolveRoleRecipients 把角色码解析为租户内符合条件的用户 ID 列表；仅 role 收件人需要。
	ResolveRoleRecipients func(ctx context.Context, tenantID string, roleCodes []string) ([]string, error)
}

func (d *Dispatcher) Run(ctx context.Context) {
	if d.Store == nil {
		return
	}
	ticker := time.NewTicker(d.Poll)
	defer ticker.Stop()
	for {
		if err := d.dispatchOne(ctx); err != nil && ctx.Err() == nil && d.Logger != nil {
			d.Logger.Error("dispatch contract notification to platform", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (d *Dispatcher) dispatchOne(ctx context.Context) error {
	delivery, found, err := d.Store.ClaimNotificationDelivery(ctx)
	if err != nil || !found {
		return err
	}
	recipients, err := d.resolveRecipients(ctx, delivery)
	if err != nil {
		return d.fail(ctx, delivery, fmt.Errorf("resolve notification recipients: %w", err))
	}
	if len(recipients) == 0 {
		// 角色在目录中已无符合条件的人员，无可送达对象，视为完成以避免无限重试。
		return d.Store.MarkNotificationDelivered(ctx, delivery.ID)
	}
	payload := ingestionEventPayload{
		EventID:           delivery.DedupeKey,
		EventType:         delivery.NotificationType,
		NotificationScope: "CROSS_SYSTEM",
		Priority:          priorityFor(delivery.NotificationType),
		Title:             delivery.Title,
		Content:           delivery.Content,
		TargetURL:         targetURLFor(delivery),
		ReferenceType:     referenceTypeFor(delivery),
		ReferenceID:       referenceIDFor(delivery),
		IdempotencyKey:    delivery.DedupeKey,
		Recipients:        recipients,
		OccurredAt:        delivery.CreatedAt,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return d.fail(ctx, delivery, fmt.Errorf("encode notification payload: %w", err))
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(d.BaseURL, "/")+"/api/v1/notifications/events", bytes.NewReader(body))
	if err != nil {
		return d.fail(ctx, delivery, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if d.TokenSource != nil {
		token, tokenErr := d.TokenSource(ctx)
		if tokenErr != nil {
			return d.fail(ctx, delivery, fmt.Errorf("fetch notification token: %w", tokenErr))
		}
		request.Header.Set("Authorization", "Bearer "+token)
	}
	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return d.fail(ctx, delivery, err)
	}
	defer response.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return d.Store.MarkNotificationDelivered(ctx, delivery.ID)
	}
	return d.fail(ctx, delivery, fmt.Errorf("platform notification API returned %d: %s", response.StatusCode, strings.TrimSpace(string(responseBody))))
}

func (d *Dispatcher) resolveRecipients(ctx context.Context, delivery Delivery) ([]string, error) {
	if delivery.RecipientRoleCode != nil && strings.TrimSpace(*delivery.RecipientRoleCode) != "" {
		if d.ResolveRoleRecipients == nil {
			return nil, fmt.Errorf("role recipient requires a recipient resolver")
		}
		return d.ResolveRoleRecipients(ctx, delivery.TenantID, []string{*delivery.RecipientRoleCode})
	}
	if delivery.RecipientUserID != nil && strings.TrimSpace(*delivery.RecipientUserID) != "" {
		return []string{*delivery.RecipientUserID}, nil
	}
	return nil, nil
}

func (d *Dispatcher) fail(ctx context.Context, delivery Delivery, err error) error {
	dead := delivery.Attempts >= d.MaxAttempts
	if markErr := d.Store.MarkNotificationFailed(ctx, delivery.ID, err.Error(), delivery.Attempts, dead); markErr != nil {
		return fmt.Errorf("delivery error: %v; persist retry: %w", err, markErr)
	}
	return err
}

type ingestionEventPayload struct {
	EventID           string     `json:"event_id"`
	EventType         string     `json:"event_type"`
	NotificationScope string     `json:"notification_scope"`
	Priority          string     `json:"priority"`
	Title             string     `json:"title"`
	Content           string     `json:"content"`
	TargetURL         string     `json:"target_url"`
	ReferenceType     string     `json:"reference_type"`
	ReferenceID       string     `json:"reference_id"`
	IdempotencyKey    string     `json:"idempotency_key"`
	Recipients        []string   `json:"recipient_user_ids"`
	OccurredAt        time.Time  `json:"occurred_at"`
	ExpiresAt         *time.Time `json:"expires_at"`
}

func priorityFor(notificationType string) string {
	switch notificationType {
	case "signing_pending", "expired":
		return "HIGH"
	default:
		return "NORMAL"
	}
}

func targetURLFor(delivery Delivery) string {
	if delivery.ContractID != nil && *delivery.ContractID != "" {
		return "/contract_management/contracts/" + *delivery.ContractID
	}
	if delivery.ApprovalID != nil && *delivery.ApprovalID != "" {
		return "/contract_management/approvals/" + *delivery.ApprovalID
	}
	return ""
}

func referenceTypeFor(delivery Delivery) string {
	if delivery.ContractID != nil && *delivery.ContractID != "" {
		return "CONTRACT"
	}
	if delivery.ApprovalID != nil && *delivery.ApprovalID != "" {
		return "APPROVAL"
	}
	return ""
}

func referenceIDFor(delivery Delivery) string {
	if delivery.ContractID != nil && *delivery.ContractID != "" {
		return *delivery.ContractID
	}
	if delivery.ApprovalID != nil && *delivery.ApprovalID != "" {
		return *delivery.ApprovalID
	}
	return ""
}
