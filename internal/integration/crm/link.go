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

// OpportunityLinkCallback 是合同接入核对完成后回写 CRM 的稳定协议。
// SyncVersion 使用接入记录版本，CRM 据此执行幂等和并发版本校验。
type OpportunityLinkCallback struct {
	EventID        string     `json:"event_id"`
	IntakeID       string     `json:"intake_id"`
	OpportunityID  uint64     `json:"opportunity_id"`
	ContractID     string     `json:"contract_id,omitempty"`
	ContractNumber string     `json:"contract_number"`
	Status         string     `json:"status"`
	LinkedAt       *time.Time `json:"linked_at"`
	SyncVersion    uint64     `json:"sync_version"`
}

// EncodeOpportunityLinkCallback 将合同接入记录转换为 CRM 回调协议。
// 确认关联时必须带确认时间；异常关联保留空时间和空正式合同 ID。
func EncodeOpportunityLinkCallback(item application.OpportunityIntake) ([]byte, error) {
	contractNumber := strings.TrimSpace(item.ContractNumber)
	if contractNumber == "" {
		contractNumber = strings.TrimSpace(item.ContractRef)
	}
	linkedAt := item.ReviewedAt
	if item.Status != application.OpportunityIntakeLinkConfirmed {
		linkedAt = nil
	}
	return json.Marshal(OpportunityLinkCallback{
		EventID: item.EventID, IntakeID: item.IntakeID, OpportunityID: item.OpportunityID,
		ContractID: item.ContractID, ContractNumber: contractNumber, Status: item.Status,
		LinkedAt: linkedAt, SyncVersion: item.Version,
	})
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
	body, err := EncodeOpportunityLinkCallback(item)
	if err != nil {
		return err
	}
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
