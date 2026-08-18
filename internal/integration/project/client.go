package project

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type Delivery struct {
	ID       string
	TenantID string
	Payload  []byte
	Attempts uint
}

type Store interface {
	ClaimProjectDelivery(context.Context) (Delivery, bool, error)
	MarkProjectDeliveryDelivered(context.Context, string) error
	MarkProjectDeliveryFailed(context.Context, string, string, uint, bool) error
}

type deliveryReconciler interface {
	ReconcileProjectDeliveries(context.Context) (int, error)
}

type Dispatcher struct {
	Store       Store
	BaseURL     string
	MaxAttempts uint
	Poll        time.Duration
	Client      *http.Client
	Logger      *slog.Logger
	// TokenSource 为内部投递提供机器访问令牌（H4：项目侧来源校验强制开启后必配）。
	// 为空时不携带 Authorization（仅限未启用投递或本地测试）。
	TokenSource func(context.Context) (string, error)
}

func (d *Dispatcher) Run(ctx context.Context) {
	if d.Store == nil {
		return
	}
	if count, err := d.reconcile(ctx); err != nil {
		if d.Logger != nil {
			d.Logger.Error("reconcile historical contract deliveries", "error", err)
		}
	} else if count > 0 && d.Logger != nil {
		d.Logger.Info("historical contract deliveries queued", "count", count)
	}
	ticker := time.NewTicker(d.Poll)
	defer ticker.Stop()
	for {
		if err := d.dispatchOne(ctx); err != nil && ctx.Err() == nil && d.Logger != nil {
			d.Logger.Error("dispatch contract activation to project management", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (d *Dispatcher) reconcile(ctx context.Context) (int, error) {
	reconciler, ok := d.Store.(deliveryReconciler)
	if !ok {
		return 0, nil
	}
	return reconciler.ReconcileProjectDeliveries(ctx)
}

func (d *Dispatcher) dispatchOne(ctx context.Context) error {
	delivery, found, err := d.Store.ClaimProjectDelivery(ctx)
	if err != nil || !found {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(d.BaseURL, "/")+"/internal/v1/contracts/activate", bytes.NewReader(delivery.Payload))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Contract-Delivery-ID", delivery.ID)
		req.Header.Set("X-Contract-Tenant-ID", delivery.TenantID)
		if d.TokenSource != nil {
			// 令牌获取失败必须走与 HTTP 失败相同的重试/死信持久化路径，避免投递被静默丢失。
			token, tokenErr := d.TokenSource(ctx)
			if tokenErr != nil {
				err = fmt.Errorf("fetch project integration token: %w", tokenErr)
			} else {
				req.Header.Set("Authorization", "Bearer "+token)
			}
		}
		if err == nil {
			client := d.Client
			if client == nil {
				client = &http.Client{Timeout: 15 * time.Second}
			}
			var response *http.Response
			response, err = client.Do(req)
			if err == nil {
				defer response.Body.Close()
				body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
				if response.StatusCode >= 200 && response.StatusCode < 300 {
					return d.Store.MarkProjectDeliveryDelivered(ctx, delivery.ID)
				}
				err = fmt.Errorf("project API returned %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
			}
		}
	}
	dead := delivery.Attempts >= d.MaxAttempts
	if markErr := d.Store.MarkProjectDeliveryFailed(ctx, delivery.ID, err.Error(), delivery.Attempts, dead); markErr != nil {
		return fmt.Errorf("delivery error: %v; persist retry: %w", err, markErr)
	}
	return err
}
