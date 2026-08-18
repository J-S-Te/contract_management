package crm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
)

type LinkDelivery struct {
	ID, TenantID string
	Payload      []byte
	Attempts     uint
}

type LinkStore interface {
	ClaimOpportunityLink(context.Context) (LinkDelivery, bool, error)
	MarkOpportunityLinkDelivered(context.Context, string) error
	MarkOpportunityLinkFailed(context.Context, string, string, uint, bool) error
}

// Dispatcher durably delivers confirmed contract links. Claimed rows are
// recoverable after a process crash and failed calls use exponential backoff.
type Dispatcher struct {
	Store          LinkStore
	BaseURL, Token string
	MaxAttempts    uint
	Poll           time.Duration
	Client         *http.Client
}

func (d *Dispatcher) Run(ctx context.Context) {
	if d.Store == nil || strings.TrimSpace(d.BaseURL) == "" {
		return
	}
	if d.Poll <= 0 {
		d.Poll = 2 * time.Second
	}
	if d.MaxAttempts == 0 {
		d.MaxAttempts = 20
	}
	ticker := time.NewTicker(d.Poll)
	defer ticker.Stop()
	for {
		_ = d.dispatchOne(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (d *Dispatcher) dispatchOne(ctx context.Context) error {
	delivery, found, err := d.Store.ClaimOpportunityLink(ctx)
	if err != nil || !found {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(d.BaseURL, "/")+"/api/v1/internal/opportunities/"+opportunityIDFromPayload(delivery.Payload)+"/contract-link", bytes.NewReader(delivery.Payload))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", eventIDFromPayload(delivery.Payload))
		if strings.TrimSpace(d.Token) != "" {
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(d.Token))
		}
		client := d.Client
		if client == nil {
			client = &http.Client{Timeout: 15 * time.Second}
		}
		resp, callErr := client.Do(req)
		if callErr == nil {
			defer resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return d.Store.MarkOpportunityLinkDelivered(ctx, delivery.ID)
			}
			callErr = fmt.Errorf("CRM API returned %d", resp.StatusCode)
		}
		err = callErr
	}
	dead := delivery.Attempts >= d.MaxAttempts
	if markErr := d.Store.MarkOpportunityLinkFailed(ctx, delivery.ID, err.Error(), delivery.Attempts, dead); markErr != nil {
		return fmt.Errorf("link delivery error: %v; persist retry: %w", err, markErr)
	}
	return err
}

func opportunityIDFromPayload(payload []byte) string {
	var v struct {
		OpportunityID uint64 `json:"opportunity_id"`
	}
	_ = json.Unmarshal(payload, &v)
	return fmt.Sprint(v.OpportunityID)
}
func eventIDFromPayload(payload []byte) string {
	var v struct {
		EventID string `json:"event_id"`
	}
	_ = json.Unmarshal(payload, &v)
	return v.EventID
}

// LinkNotifier posts confirmed contract links to the CRM callback endpoint.
// The bounded retry keeps transient CRM failures recoverable without making
// the review request hang indefinitely; a durable outbox can wrap this seam.
type LinkNotifier struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

func (n *LinkNotifier) NotifyOpportunityLink(ctx context.Context, item application.OpportunityIntake) error {
	if strings.TrimSpace(n.BaseURL) == "" {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"event_id": item.EventID, "intake_id": item.IntakeID, "contract_id": item.ContractID, "contract_number": item.ContractNumber, "status": item.Status, "linked_at": time.Now().UTC(), "sync_version": item.Version})
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(n.BaseURL, "/")+"/api/v1/internal/opportunities/"+fmt.Sprint(item.OpportunityID)+"/contract-link", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if strings.TrimSpace(n.Token) != "" {
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(n.Token))
		}
		req.Header.Set("Idempotency-Key", item.EventID)
		resp, err := n.Client.Do(req)
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		last = err
		time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
	}
	return last
}
