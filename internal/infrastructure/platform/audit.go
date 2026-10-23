package platform

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// AuditEvent 有意排除请求体和凭据，是平台
// 审计摄取契约接受的最小业务摘要。
type AuditEvent struct {
	// EventID/OccurredAt 由事务内 outbox 提供；为空时兼容旧调用方现场生成。
	EventID                                              string
	OccurredAt                                           time.Time
	ActorID, ActorName, Action, ResourceType, ResourceID string
	RequestID, CorrelationID, Result, ReasonCode         string
	// RiskLevel 为 LOW/MEDIUM/HIGH/CRITICAL；为空时按 LOW 上报。
	RiskLevel string
	// 统一审计来源协议（AUD-IP-2026）：
	//   LoginIP  创建会话时观察到的客户端地址（登录态固定，可能为空）
	//   ClientIP 本次业务操作的客户端地址
	//   IPSource 地址来源：TRUSTED_PROXY / DIRECT / UNKNOWN
	LoginIP, ClientIP, IPSource string
	// UserLoginIP 是旧协议字段，继续按"登录地址"语义上报以保证滚动升级兼容。
	UserLoginIP string
}

type AuditReporter interface {
	Report(context.Context, AuditEvent) error
}

type AuditClient struct {
	baseURL, clientID, clientSecret, applicationCode, environmentCode string
	client                                                            *http.Client
	mu                                                                sync.Mutex
	token                                                             string
	expiresAt                                                         time.Time
}

func NewAuditReporter(baseURL, clientID, clientSecret, applicationCode, environmentCode string) AuditReporter {
	if clientID == "" || clientSecret == "" || applicationCode == "" || environmentCode == "" {
		return nil
	}
	return &AuditClient{baseURL: strings.TrimRight(baseURL, "/"), clientID: clientID, clientSecret: clientSecret, applicationCode: applicationCode, environmentCode: environmentCode, client: &http.Client{Timeout: 5 * time.Second}}
}

func (c *AuditClient) Report(ctx context.Context, event AuditEvent) error {
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	requestID := event.RequestID
	if requestID == "" {
		requestID = ulid.Make().String()
	}
	correlationID := event.CorrelationID
	if correlationID == "" {
		correlationID = requestID
	}
	traceID, parentID, err := traceIDs()
	if err != nil {
		return err
	}
	eventID := event.EventID
	if eventID == "" {
		eventID = ulid.Make().String()
	}
	occurredAt := event.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	payload := map[string]any{
		"event_id": eventID, "occurred_at": occurredAt.UTC().Format(time.RFC3339Nano),
		"application_code": c.applicationCode, "environment_code": c.environmentCode,
		"actor_type": actorType(event.ActorID), "action": event.Action, "resource_type": event.ResourceType,
		"request_id": requestID, "trace_id": traceID, "correlation_id": correlationID,
		"result": event.Result, "risk_level": riskLevelOrDefault(event.RiskLevel), "metadata": map[string]any{"http_status": event.ReasonCode},
	}
	if event.ActorID != "" {
		payload["actor_id"] = event.ActorID
	}
	if event.ActorName != "" {
		payload["actor_name"] = event.ActorName
	}
	if event.ResourceID != "" {
		payload["resource_id"] = event.ResourceID
	}
	if event.ReasonCode != "" {
		payload["reason_code"] = event.ReasonCode
	}
	loginIP := event.LoginIP
	if loginIP == "" {
		loginIP = event.UserLoginIP
	}
	if loginIP != "" {
		payload["login_ip"] = loginIP
		payload["user_login_ip"] = loginIP
	}
	if event.ClientIP != "" {
		payload["client_ip"] = event.ClientIP
	}
	if event.IPSource != "" {
		payload["ip_source"] = event.IPSource
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/audit/events", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Request-ID", requestID)
	req.Header.Set("X-Correlation-ID", correlationID)
	req.Header.Set("traceparent", "00-"+traceID+"-"+parentID+"-01")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("platform audit returned %d", resp.StatusCode)
	}
	return nil
}

func (c *AuditClient) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.expiresAt) > 30*time.Second {
		return c.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {"audit.ingest"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.clientID, c.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return "", fmt.Errorf("platform token returned %d", resp.StatusCode)
	}
	var token struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		Scope       string `json:"scope"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&token); err != nil {
		return "", err
	}
	if token.AccessToken == "" || !strings.EqualFold(token.TokenType, "bearer") || !hasScope(token.Scope, "audit.ingest") {
		return "", fmt.Errorf("platform token missing audit.ingest scope")
	}
	if token.ExpiresIn <= 0 {
		return "", fmt.Errorf("platform token has invalid expiry")
	}
	c.token, c.expiresAt = token.AccessToken, time.Now().Add(time.Duration(token.ExpiresIn)*time.Second)
	return c.token, nil
}

func hasScope(scopes, wanted string) bool {
	for _, scope := range strings.Fields(scopes) {
		if scope == wanted {
			return true
		}
	}
	return false
}
func actorType(actorID string) string {
	if actorID == "" {
		return "SYSTEM"
	}
	return "USER"
}
func riskLevelOrDefault(level string) string {
	level = strings.ToUpper(strings.TrimSpace(level))
	if level != "LOW" && level != "MEDIUM" && level != "HIGH" && level != "CRITICAL" {
		return "LOW"
	}
	return level
}
func traceIDs() (string, string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(raw[:16]), hex.EncodeToString(raw[16:]), nil
}
