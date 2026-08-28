package platform

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

const backchannelLogoutEvent = "http://schemas.openid.net/event/backchannel-logout"

// BackchannelLogoutStore 为标准 OIDC 注销令牌提供持久重放记录和会话撤销能力。
type BackchannelLogoutStore interface {
	OIDCStore
	ProcessBackchannelLogout(context.Context, string, string, string, time.Time) (bool, error)
	ClaimBackchannelLogout(context.Context, string, time.Time) (bool, error)
	ReleaseBackchannelLogout(context.Context, string) error
	RevokeSessionsForSubject(context.Context, string, string, time.Time) error
}

type backchannelLogoutClaims struct {
	Issuer   string                     `json:"iss"`
	Subject  string                     `json:"sub"`
	Session  string                     `json:"sid"`
	Audience interface{}                `json:"aud"`
	Issued   int64                      `json:"iat"`
	Expires  int64                      `json:"exp"`
	JTI      string                     `json:"jti"`
	Events   map[string]json.RawMessage `json:"events"`
}

// BackchannelLogout 接收并验证 OIDC Back-Channel Logout logout_token，然后撤销本地会话。
// 该接口只接受表单中的 logout_token，普通 ID Token 会因缺少事件声明而拒绝。
func (a *OIDCAuthenticator) BackchannelLogout(writer http.ResponseWriter, request *http.Request) {
	store, ok := a.store.(BackchannelLogoutStore)
	if !ok {
		http.Error(writer, "back-channel logout storage is unavailable", http.StatusServiceUnavailable)
		return
	}
	if request.Method != http.MethodPost {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := request.ParseForm(); err != nil {
		http.Error(writer, "invalid form", http.StatusBadRequest)
		return
	}
	raw := strings.TrimSpace(request.Form.Get("logout_token"))
	if raw == "" || len(raw) > 64*1024 {
		http.Error(writer, "logout_token is required", http.StatusBadRequest)
		return
	}
	idToken, err := a.verifier.Verify(request.Context(), raw)
	if err != nil {
		http.Error(writer, "invalid logout_token", http.StatusUnauthorized)
		return
	}
	var claims backchannelLogoutClaims
	if err := idToken.Claims(&claims); err != nil || !validBackchannelClaims(claims, a.options.ClientID, a.now().UTC()) {
		http.Error(writer, "invalid logout_token claims", http.StatusUnauthorized)
		return
	}
	now := a.now().UTC()
	claimed, err := store.ProcessBackchannelLogout(request.Context(), a.options.TenantID, claims.Subject, claims.JTI, now)
	if err != nil {
		http.Error(writer, "logout storage unavailable", http.StatusServiceUnavailable)
		return
	}
	if !claimed {
		writer.WriteHeader(http.StatusOK)
		return
	}
	writer.WriteHeader(http.StatusOK)
}

type backchannelLogoutProcessor interface {
	ProcessBackchannelLogout(context.Context, string, string, string, time.Time) (bool, error)
}

func processBackchannelLogout(ctx context.Context, store backchannelLogoutProcessor, tenantID, subject, jti string, now time.Time) error {
	_, err := store.ProcessBackchannelLogout(ctx, tenantID, subject, jti, now)
	return err
}

func validBackchannelClaims(claims backchannelLogoutClaims, clientID string, now time.Time) bool {
	if claims.Subject == "" || claims.JTI == "" || claims.Issued <= 0 || claims.Expires <= now.Unix() || claims.Expires-claims.Issued > 300 {
		return false
	}
	event, ok := claims.Events[backchannelLogoutEvent]
	if !ok {
		return false
	}
	var eventObject map[string]json.RawMessage
	if json.Unmarshal(event, &eventObject) != nil || len(eventObject) != 0 {
		return false
	}
	switch audience := claims.Audience.(type) {
	case string:
		return audience == clientID
	case []interface{}:
		for _, item := range audience {
			if value, ok := item.(string); ok && value == clientID {
				return true
			}
		}
	}
	return false
}

func backchannelLogoutJTIHash(jti string) []byte {
	sum := sha256.Sum256([]byte(jti))
	return sum[:]
}

var errBackchannelLogoutReplay = errors.New("back-channel logout token replay")

// BackchannelLogoutReplayError 判断错误是否表示 logout_token 已经处理过。
func BackchannelLogoutReplayError(err error) bool { return errors.Is(err, errBackchannelLogoutReplay) }
