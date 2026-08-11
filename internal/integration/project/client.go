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
	dead := delivery.Attempts >= d.MaxAttempts
	if markErr := d.Store.MarkProjectDeliveryFailed(ctx, delivery.ID, err.Error(), delivery.Attempts, dead); markErr != nil {
		return fmt.Errorf("delivery error: %v; persist retry: %w", err, markErr)
	}
	return err
}
