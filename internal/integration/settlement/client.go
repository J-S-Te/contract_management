package settlement

import (
	"bytes"
	"context"
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

func (d *Dispatcher) Run(ctx context.Context) {
	if d.Store == nil {
		return
	}
	if count, err := d.reconcile(ctx); err != nil {
		if d.Logger != nil {
			d.Logger.Error("reconcile historical financial effective deliveries", "error", err)
		}
	} else if count > 0 && d.Logger != nil {
		d.Logger.Info("historical financial effective deliveries queued", "count", count)
	}
	ticker := time.NewTicker(d.Poll)
	defer ticker.Stop()
	for {
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

func (d *Dispatcher) reconcile(ctx context.Context) (int, error) {
	if d.LicenseGate != nil {
		if err := d.LicenseGate.Check(ctx, core.MUTATE_BUSINESS); err != nil {
			return 0, err
		}
	}
	reconciler, ok := d.Store.(deliveryReconciler)
	if !ok {
		return 0, nil
	}
	return reconciler.ReconcileFinancialEffectives(ctx)
}

func (d *Dispatcher) dispatchOne(ctx context.Context) error {
	if d.LicenseGate != nil {
		if err := d.LicenseGate.Check(ctx, core.MUTATE_BUSINESS); err != nil {
			return err
		}
	}
	delivery, found, err := d.Store.ClaimFinancialEffectiveDelivery(ctx)
	if err != nil || !found {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(d.BaseURL, "/")+"/internal/v1/settlement/events/contracts",
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
					return d.Store.MarkFinancialEffectiveDelivered(ctx, delivery.ID)
				}
				err = fmt.Errorf("settlement API returned %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
			}
		}
	}
	dead := delivery.Attempts >= d.MaxAttempts
	if markErr := d.Store.MarkFinancialEffectiveFailed(ctx, delivery.ID, err.Error(), delivery.Attempts, dead); markErr != nil {
		return fmt.Errorf("delivery error: %v; persist retry: %w", err, markErr)
	}
	return err
}
