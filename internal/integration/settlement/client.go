package settlement

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

	core "github.com/J-S-Te/license-core"
)

// 合同 → 结算的财务生效事件投递。
//
// 结算侧固定入站端点为 POST /internal/v1/settlement/events/contracts，
// 事件类型 contract.financial_effective.v1，需要能解析出可信租户的机器 Bearer 令牌；
// 来源标识由结算侧硬编码为 contract_management，投递方不需要也不应该声明。
// 本投递器与 internal/integration/project 保持同一形态（认领/成功/失败/死信 + 许可门禁）。

type Delivery struct {
	ID       string
	TenantID string
	Payload  []byte
	Attempts uint
}

type Store interface {
	ClaimFinancialEffectiveDelivery(context.Context) (Delivery, bool, error)
	MarkFinancialEffectiveDelivered(context.Context, string) error
	MarkFinancialEffectiveFailed(context.Context, string, string, uint, bool) error
}

type responsibilityStore interface {
	ClaimContractResponsibilityDelivery(context.Context) (Delivery, bool, error)
	MarkContractResponsibilityDelivered(context.Context, string) error
	MarkContractResponsibilityFailed(context.Context, string, string, uint, bool) error
	ReconcileContractResponsibilities(context.Context) (int, error)
}

type deliveryReconciler interface {
	ReconcileFinancialEffectives(context.Context) (int, error)
}

type Dispatcher struct {
	LicenseGate interface {
		Check(context.Context, core.Operation) error
	}
	Store       Store
	BaseURL     string
	MaxAttempts uint
	Poll        time.Duration
	Client      *http.Client
	Logger      *slog.Logger
	// TokenSource 提供机器访问令牌。为空时不携带 Authorization——仅允许在未启用
	// 结算集成或本地测试时出现；结算侧会以 401 失败关闭，不会被静默接受。
	TokenSource func(context.Context) (string, error)
}

// Run 运行财务和责任事件的持久化投递循环。
// 参数：ctx 为进程生命周期。
// 返回值：无。
func (d *Dispatcher) Run(ctx context.Context) {
	if d.Store == nil {
		return
	}
	reconciled := false

	ticker := time.NewTicker(d.Poll)
	defer ticker.Stop()
	for {
		if !reconciled {
			count, err := d.reconcile(ctx)
			if err != nil {
				if d.Logger != nil {
					d.Logger.Error("reconcile settlement deliveries", "error", err)
				}
			} else {
				reconciled = true
				if count > 0 && d.Logger != nil {
					d.Logger.Info("historical settlement deliveries queued", "count", count)
				}
			}
		}

		if err := d.dispatchOne(ctx); err != nil && ctx.Err() == nil && d.Logger != nil {
			d.Logger.Error("dispatch financial effective event to settlement", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// reconcile 补建现有合同缺失的责任及财务事件。
// 参数：ctx 控制存储操作。
// 返回值：新增数量及存储或许可错误。
func (d *Dispatcher) reconcile(ctx context.Context) (int, error) {
	if d.LicenseGate != nil {
		if err := d.LicenseGate.Check(ctx, core.MUTATE_BUSINESS); err != nil {
			return 0, err
		}
	}
	responsibilityCount := 0
	if store, ok := d.Store.(responsibilityStore); ok {
		var err error
		responsibilityCount, err = store.ReconcileContractResponsibilities(ctx)
		if err != nil {
			return 0, err
		}
	}
	reconciler, ok := d.Store.(deliveryReconciler)
	if !ok {
		return responsibilityCount, nil
	}
	count, err := reconciler.ReconcileFinancialEffectives(ctx)
	return responsibilityCount + count, err
}

// dispatchOne 投递财务事件并独立处理一条责任事件。
// 参数：ctx 控制请求及持久化重试。
// 返回值：投递、许可或重试记录错误。
func (d *Dispatcher) dispatchOne(ctx context.Context) error {
	if store, ok := d.Store.(responsibilityStore); ok {
		if d.LicenseGate != nil {
			if err := d.LicenseGate.Check(ctx, core.MUTATE_BUSINESS); err != nil {
				return err
			}
		}
		delivery, found, err := store.ClaimContractResponsibilityDelivery(ctx)
		if err != nil {
			return err
		}
		if found {
			if err := d.deliver(ctx, delivery, store.MarkContractResponsibilityDelivered, store.MarkContractResponsibilityFailed); err != nil {
				return err
			}
		}
	}
	if d.LicenseGate != nil {
		if err := d.LicenseGate.Check(ctx, core.MUTATE_BUSINESS); err != nil {
			return err
		}
	}
	delivery, found, err := d.Store.ClaimFinancialEffectiveDelivery(ctx)
	if err != nil || !found {
		return err
	}
	return d.deliver(ctx, delivery, d.Store.MarkFinancialEffectiveDelivered, d.Store.MarkFinancialEffectiveFailed)
}

// deliver 按事件类型发送冻结载荷并持久记录成功或失败。
// 参数：ctx 控制请求，delivery 为队列行，delivered 和 failed 写回各自队列。
// 返回值：网络、载荷或持久化错误。
func (d *Dispatcher) deliver(ctx context.Context, delivery Delivery, delivered func(context.Context, string) error, failed func(context.Context, string, string, uint, bool) error) error {
	var event struct {
		EventType string `json:"event_type"`
	}
	_ = json.Unmarshal(delivery.Payload, &event)
	endpoint := "/internal/v1/settlement/events/contracts"
	if event.EventType == "contract.responsibility_changed.v1" {
		endpoint = "/internal/v1/settlement/events/contract-responsibility"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(d.BaseURL, "/")+endpoint,
		bytes.NewReader(delivery.Payload))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Financial-Effective-Delivery-ID", delivery.ID)
		req.Header.Set("X-Financial-Effective-Tenant-ID", delivery.TenantID)
		if d.TokenSource != nil {
			// 令牌获取失败必须走与 HTTP 失败相同的重试/死信持久化路径，避免投递被静默丢失。
			token, tokenErr := d.TokenSource(ctx)
			if tokenErr != nil {
				err = fmt.Errorf("fetch settlement integration token: %w", tokenErr)
			} else {
				req.Header.Set("Authorization", "Bearer "+token)
			}
		}
		if err == nil {
			client := d.Client
			if client == nil {
				client = &http.Client{Timeout: 15 * time.Second}
			}
			if d.LicenseGate != nil {
				if licenseErr := d.LicenseGate.Check(ctx, core.MUTATE_BUSINESS); licenseErr != nil {
					return licenseErr
				}
			}
			var response *http.Response
			response, err = client.Do(req)
			if err == nil {
				defer response.Body.Close()
				body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
				if response.StatusCode >= 200 && response.StatusCode < 300 {
					return delivered(ctx, delivery.ID)
				}
				err = fmt.Errorf("settlement API returned %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
			}
		}
	}
	dead := delivery.Attempts >= d.MaxAttempts
	if markErr := failed(ctx, delivery.ID, err.Error(), delivery.Attempts, dead); markErr != nil {
		return fmt.Errorf("delivery error: %v; persist retry: %w", err, markErr)
	}
	return err
}
