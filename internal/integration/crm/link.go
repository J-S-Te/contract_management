package crm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	core "github.com/J-S-Te/license-core"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/lifecycle"
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

// Dispatcher 持久投递已确认合同关联。已认领记录可在
// 进程崩溃后恢复，失败调用使用指数退避。
type Dispatcher struct {
	LicenseGate application.LicenseChecker
	Store       LinkStore
	BaseURL     string
	MaxAttempts uint
	Poll        time.Duration
	Client      *http.Client
	Logger      *slog.Logger
	// TokenSource 提供 CRM 机器访问令牌（平台 client_credentials，scope 固定
	// opportunity.signed.write）。令牌获取失败与 HTTP 失败走同一重试/死信
	// 持久化路径，不得在缺少令牌时发送请求（否则 CRM 以 401 反复拒绝）。
	TokenSource func(context.Context) (string, error)
	// Now/Nonce 仅用于测试替换时间戳与 nonce 生成；生产使用系统时间与
	// 32 字节 crypto/rand 的 base64url 编码。
	Now   func() time.Time
	Nonce func() (string, error)
}

// Run 运行关联回写的持久投递循环。
// 参数：ctx 为进程生命周期。
// 返回值：无。
func (d *Dispatcher) Run(ctx context.Context) {
	if d.Store == nil {
		return
	}
	// 未配置 BaseURL 表示该子系统没有启用 CRM 关联回写。启动日志必须明确说明，
	// 避免“审核完成但从未回写”在生产上完全静默。
	if strings.TrimSpace(d.BaseURL) == "" {
		if d.Logger != nil {
			d.Logger.Info("CRM link integration disabled: CRM_LINK_BASE_URL is empty")
		}
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
		// 收到 SIGTERM 后不再认领新记录；在途记录的终态由 dispatchOne 用独立 ctx 落库。
		if ctx.Err() != nil {
			return
		}
		if err := d.dispatchOne(ctx); err != nil && ctx.Err() == nil && d.Logger != nil {
			d.Logger.Error("dispatch contract opportunity link to CRM", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (d *Dispatcher) dispatchOne(ctx context.Context) error {
	if d.LicenseGate != nil {
		if err := d.LicenseGate.Check(ctx, core.MUTATE_BUSINESS); err != nil {
			return err
		}
	}
	delivery, found, err := d.Store.ClaimOpportunityLink(ctx)
	if err != nil || !found {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(d.BaseURL, "/")+"/api/v1/internal/opportunities/"+opportunityIDFromPayload(delivery.Payload)+"/contract-link", bytes.NewReader(delivery.Payload))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", eventIDFromPayload(delivery.Payload))
		if err = authorizeCRMIntegrationRequest(ctx, req, d.TokenSource, d.Now, d.Nonce); err == nil {
			client := d.Client
			if client == nil {
				client = &http.Client{Timeout: 15 * time.Second}
			}
			if d.LicenseGate != nil {
				if licenseErr := d.LicenseGate.Check(ctx, core.MUTATE_BUSINESS); licenseErr != nil {
					return licenseErr
				}
			}
			resp, callErr := client.Do(req)
			if callErr == nil {
				defer resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					// 退出期间也必须写入终态：不能把已投递记录留在 CLAIMED 等租约。
					persistCtx, cancel := lifecycle.PersistContext(ctx)
					defer cancel()
					return d.Store.MarkOpportunityLinkDelivered(persistCtx, delivery.ID)
				}
				callErr = fmt.Errorf("CRM API returned %d", resp.StatusCode)
			}
			err = callErr
		}
	}
	dead := delivery.Attempts >= d.MaxAttempts
	persistCtx, cancel := lifecycle.PersistContext(ctx)
	defer cancel()
	if markErr := d.Store.MarkOpportunityLinkFailed(persistCtx, delivery.ID, err.Error(), delivery.Attempts, dead); markErr != nil {
		return fmt.Errorf("link delivery error: %v; persist retry: %w", err, markErr)
	}
	return err
}

// authorizeCRMIntegrationRequest 为 CRM 机器接入补齐平台签发的 Bearer 令牌
// 与重放防护头。CRM 在验签层同时校验 scope、时间戳窗口与一次性 nonce，
// 缺任一都会被 401 拒绝。
func authorizeCRMIntegrationRequest(ctx context.Context, request *http.Request, tokenSource func(context.Context) (string, error), now func() time.Time, nonce func() (string, error)) error {
	if tokenSource == nil {
		return errors.New("CRM link integration token source is not configured")
	}
	token, err := tokenSource(ctx)
	if err != nil {
		return fmt.Errorf("obtain CRM link machine token: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return errors.New("CRM link machine token is empty")
	}
	nowFunc := time.Now
	if now != nil {
		nowFunc = now
	}
	nonceFunc := integrationNonce
	if nonce != nil {
		nonceFunc = nonce
	}
	nonceValue, err := nonceFunc()
	if err != nil {
		return fmt.Errorf("generate CRM request nonce: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	request.Header.Set("X-Integration-Timestamp", nowFunc().UTC().Format(time.RFC3339Nano))
	request.Header.Set("X-Integration-Nonce", nonceValue)
	return nil
}

func integrationNonce() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
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

// LinkNotifier 向 CRM 回调端点发送已确认合同关联。
// 有界重试使暂时性 CRM 故障可恢复，避免
// 审核请求无限挂起；此边界可由持久化 Outbox 包裹。
type LinkNotifier struct {
	LicenseGate application.LicenseChecker
	BaseURL     string
	Client      *http.Client
	TokenSource func(context.Context) (string, error)
	Now         func() time.Time
	Nonce       func() (string, error)
}

func (n *LinkNotifier) NotifyOpportunityLink(ctx context.Context, item application.OpportunityIntake) error {
	if n.LicenseGate != nil {
		if err := n.LicenseGate.Check(ctx, core.MUTATE_BUSINESS); err != nil {
			return err
		}
	}
	if strings.TrimSpace(n.BaseURL) == "" {
		return nil
	}
	body, err := EncodeOpportunityLinkCallback(item)
	if err != nil {
		return err
	}
	client := n.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if n.LicenseGate != nil {
			if err := n.LicenseGate.Check(ctx, core.MUTATE_BUSINESS); err != nil {
				return err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(n.BaseURL, "/")+"/api/v1/internal/opportunities/"+fmt.Sprint(item.OpportunityID)+"/contract-link", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", item.EventID)
		if err := authorizeCRMIntegrationRequest(ctx, req, n.TokenSource, n.Now, n.Nonce); err != nil {
			last = err
		} else {
			resp, callErr := client.Do(req)
			if callErr == nil {
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					resp.Body.Close()
					return nil
				}
				resp.Body.Close()
				last = fmt.Errorf("CRM API returned %d", resp.StatusCode)
			} else {
				last = callErr
			}
		}
		time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
	}
	return last
}
