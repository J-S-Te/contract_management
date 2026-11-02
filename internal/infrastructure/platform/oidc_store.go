package platform

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"gorm.io/gorm"
)

type LoginTransactionRecord struct {
	StateHash              []byte
	TenantID               string
	NonceCiphertext        []byte
	CodeVerifierCiphertext []byte
	ReturnPath             string
	ExpiresAt              time.Time
	ConsumedAt             *time.Time
	CreatedAt              time.Time
}

type SessionRecord struct {
	SessionIDHash          []byte
	TenantID               string
	IdentityID             string
	PersonID               string
	PrincipalJSON          []byte
	AccessTokenCiphertext  []byte
	RefreshTokenCiphertext []byte
	IDTokenCiphertext      []byte
	AuthorizationRevision  uint64
	AuthorizationCheckedAt time.Time
	TokenExpiresAt         time.Time
	SessionExpiresAt       time.Time
	// LastSeenAt 记录任意一次已认证请求；LastInteractiveAt 只由显式浏览器交互推进。
	// 旧会话行可能为 nil，空闲基准回退到 CreatedAt（迁移已按 created_at 回填）。
	LastSeenAt        *time.Time
	LastInteractiveAt *time.Time
	RevokedAt         *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type OIDCStore interface {
	SaveLoginTransaction(context.Context, LoginTransactionRecord) error
	ConsumeLoginTransaction(context.Context, []byte, time.Time) (LoginTransactionRecord, error)
	CreateSession(context.Context, SessionRecord) error
	FindSession(context.Context, []byte, time.Time) (SessionRecord, error)
	// TouchSessionLastSeen 在会话仍有效时记录普通已认证请求；不得推进空闲基准。
	TouchSessionLastSeen(context.Context, []byte, time.Time) error
	// RecordInteraction 在会话仍处于空闲窗口内时推进显式交互时间；绝对期限不变。
	RecordInteraction(context.Context, []byte, time.Time, time.Duration) error
	UpdateSession(context.Context, SessionRecord) error
	RevokeSession(context.Context, []byte, time.Time) error
	RevokeSessionsForIdentity(context.Context, string, string, time.Time) error
	DeleteExpired(context.Context, time.Time) error
}

type gormLoginTransaction struct {
	StateHash              []byte `gorm:"column:state_hash;primaryKey"`
	TenantID               string `gorm:"column:tenant_id"`
	NonceCiphertext        []byte `gorm:"column:nonce_ciphertext"`
	CodeVerifierCiphertext []byte `gorm:"column:code_verifier_ciphertext"`
	ReturnPath             string `gorm:"column:return_path"`
	ExpiresAt              time.Time
	ConsumedAt             *time.Time
	CreatedAt              time.Time
}

func (gormLoginTransaction) TableName() string { return "con_oidc_login_transaction" }

type gormSession struct {
	SessionIDHash          []byte `gorm:"column:session_id_hash;primaryKey"`
	TenantID               string
	IdentityID             string
	PersonID               *string
	PrincipalJSON          []byte `gorm:"type:json"`
	AccessTokenCiphertext  []byte
	RefreshTokenCiphertext []byte
	IDTokenCiphertext      []byte
	AuthorizationRevision  uint64
	AuthorizationCheckedAt time.Time
	TokenExpiresAt         time.Time
	SessionExpiresAt       time.Time
	LastSeenAt             *time.Time
	LastInteractiveAt      *time.Time
	RevokedAt              *time.Time
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

func (gormSession) TableName() string { return "con_oidc_session" }

type GORMOIDCStore struct{ db *gorm.DB }

// ProcessBackchannelLogout 在单一事务中记录 JTI 并撤销 subject 会话，避免中途崩溃留下错误的已处理标记。
func (s *GORMOIDCStore) ProcessBackchannelLogout(ctx context.Context, tenantID, subject, jti string, now time.Time) (bool, error) {
	if strings.TrimSpace(jti) == "" || strings.TrimSpace(subject) == "" {
		return false, errors.New("back-channel logout subject and JTI are required")
	}
	claimed := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Exec("INSERT INTO con_oidc_backchannel_logout_replay (jti_hash, expires_at, created_at) VALUES (?, ?, ?)", backchannelLogoutJTIHash(jti), now.Add(5*time.Minute), now)
		if result.Error != nil {
			if strings.Contains(strings.ToLower(result.Error.Error()), "duplicate") {
				return nil
			}
			return result.Error
		}
		claimed = true
		return tx.Model(&gormSession{}).
			Where("tenant_id = ? AND revoked_at IS NULL AND JSON_UNQUOTE(JSON_EXTRACT(principal_json, '$.subject')) = ?", tenantID, subject).
			Updates(map[string]any{"revoked_at": now, "updated_at": now}).Error
	})
	return claimed, err
}

func NewGORMOIDCStore(db *gorm.DB) (*GORMOIDCStore, error) {
	if db == nil {
		return nil, errors.New("OIDC store database is required")
	}
	return &GORMOIDCStore{db: db}, nil
}

func (s *GORMOIDCStore) SaveLoginTransaction(ctx context.Context, record LoginTransactionRecord) error {
	return s.db.WithContext(ctx).Create(&gormLoginTransaction{
		StateHash: record.StateHash, TenantID: record.TenantID,
		NonceCiphertext: record.NonceCiphertext, CodeVerifierCiphertext: record.CodeVerifierCiphertext,
		ReturnPath: record.ReturnPath, ExpiresAt: record.ExpiresAt, CreatedAt: record.CreatedAt,
	}).Error
}

func (s *GORMOIDCStore) ConsumeLoginTransaction(ctx context.Context, stateHash []byte, now time.Time) (LoginTransactionRecord, error) {
	var result gormLoginTransaction
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		updated := tx.Model(&gormLoginTransaction{}).
			Where("state_hash = ? AND consumed_at IS NULL AND expires_at > ?", stateHash, now).
			Update("consumed_at", now)
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return ErrUnauthenticated
		}
		return tx.Where("state_hash = ?", stateHash).Take(&result).Error
	})
	if err != nil {
		return LoginTransactionRecord{}, err
	}
	return LoginTransactionRecord{StateHash: result.StateHash, TenantID: result.TenantID, NonceCiphertext: result.NonceCiphertext, CodeVerifierCiphertext: result.CodeVerifierCiphertext, ReturnPath: result.ReturnPath, ExpiresAt: result.ExpiresAt, ConsumedAt: result.ConsumedAt, CreatedAt: result.CreatedAt}, nil
}

func (s *GORMOIDCStore) CreateSession(ctx context.Context, record SessionRecord) error {
	row := sessionToGORM(record)
	return s.db.WithContext(ctx).Create(&row).Error
}

func (s *GORMOIDCStore) FindSession(ctx context.Context, hash []byte, now time.Time) (SessionRecord, error) {
	var row gormSession
	err := s.db.WithContext(ctx).Where("session_id_hash = ? AND revoked_at IS NULL AND session_expires_at > ?", hash, now).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return SessionRecord{}, ErrUnauthenticated
	}
	if err != nil {
		return SessionRecord{}, err
	}
	return sessionFromGORM(row), nil
}

// TouchSessionLastSeen 只推进普通已认证请求的观测时间。
// 参数：ctx 为请求上下文，hash 为会话 Cookie 哈希，now 为服务端当前时间。
// 返回值：会话已撤销或绝对到期返回 ErrUnauthenticated；写入冲突时复核而不是误判登出。
func (s *GORMOIDCStore) TouchSessionLastSeen(ctx context.Context, hash []byte, now time.Time) error {
	result := s.db.WithContext(ctx).Model(&gormSession{}).
		Where("session_id_hash = ? AND revoked_at IS NULL AND session_expires_at > ?", hash, now).
		Update("last_seen_at", now)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 1 {
		return nil
	}
	// MySQL 对并发请求写入相同 DATETIME(3) 值的 no-op 更新也返回零变更行，
	// 因此零行时必须复核会话是否真实失效，不能直接把无害 no-op 当成登出。
	var row gormSession
	err := s.db.WithContext(ctx).Select("session_id_hash").
		Where("session_id_hash = ? AND revoked_at IS NULL AND session_expires_at > ?", hash, now).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrUnauthenticated
	}
	return err
}

// RecordInteraction 只有显式活动接口才能调用：推进 last_interactive_at 以延长空闲窗口，
// 但绝不修改 session_expires_at（绝对期限）。
// 参数：ctx 为请求上下文，hash 为会话 Cookie 哈希，now 为交互时间，idleTimeout 为当前空闲期限。
// 返回值：会话已撤销、绝对到期或空闲到期返回 ErrUnauthenticated，不复活任何失效会话。
func (s *GORMOIDCStore) RecordInteraction(ctx context.Context, hash []byte, now time.Time, idleTimeout time.Duration) error {
	if idleTimeout <= 0 {
		return ErrUnauthenticated
	}
	idleCutoff := now.Add(-idleTimeout)
	condition := "session_id_hash = ? AND revoked_at IS NULL AND session_expires_at > ? AND COALESCE(last_interactive_at, created_at) > ?"
	result := s.db.WithContext(ctx).Model(&gormSession{}).
		Where(condition, hash, now, idleCutoff).
		Updates(map[string]any{"last_seen_at": now, "last_interactive_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 1 {
		return nil
	}
	// 并发活动 ping 写入相同 DATETIME(3) 值时同样返回零变更行：复核后再决定是否拒绝。
	var row gormSession
	err := s.db.WithContext(ctx).Select("session_id_hash").Where(condition, hash, now, idleCutoff).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrUnauthenticated
	}
	return err
}

func (s *GORMOIDCStore) UpdateSession(ctx context.Context, record SessionRecord) error {
	row := sessionToGORM(record)
	updates := map[string]any{
		"person_id": nullableString(record.PersonID), "principal_json": row.PrincipalJSON,
		"access_token_ciphertext": row.AccessTokenCiphertext, "refresh_token_ciphertext": row.RefreshTokenCiphertext,
		"id_token_ciphertext": row.IDTokenCiphertext, "authorization_revision": row.AuthorizationRevision,
		"authorization_checked_at": row.AuthorizationCheckedAt, "token_expires_at": row.TokenExpiresAt,
		"updated_at": row.UpdatedAt,
	}
	result := s.db.WithContext(ctx).Model(&gormSession{}).
		Where("session_id_hash = ? AND revoked_at IS NULL", record.SessionIDHash).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrUnauthenticated
	}
	return nil
}

func (s *GORMOIDCStore) RevokeSession(ctx context.Context, hash []byte, now time.Time) error {
	return s.db.WithContext(ctx).Model(&gormSession{}).
		Where("session_id_hash = ? AND revoked_at IS NULL", hash).
		Updates(map[string]any{"revoked_at": now, "updated_at": now}).Error
}

func (s *GORMOIDCStore) RevokeSessionsForIdentity(ctx context.Context, tenantID, identityID string, now time.Time) error {
	return s.db.WithContext(ctx).Model(&gormSession{}).
		Where("tenant_id = ? AND identity_id = ? AND revoked_at IS NULL", tenantID, identityID).
		Updates(map[string]any{"revoked_at": now, "updated_at": now}).Error
}

// ClaimBackchannelLogout 以唯一 JTI 持久化注销事件，防止重复投递重复执行。
func (s *GORMOIDCStore) ClaimBackchannelLogout(ctx context.Context, jti string, now time.Time) (bool, error) {
	if jti == "" {
		return false, errors.New("back-channel logout JTI is required")
	}
	result := s.db.WithContext(ctx).Exec("INSERT INTO con_oidc_backchannel_logout_replay (jti_hash, expires_at, created_at) VALUES (?, ?, ?)", backchannelLogoutJTIHash(jti), now.Add(5*time.Minute), now)
	if result.Error != nil {
		if strings.Contains(strings.ToLower(result.Error.Error()), "duplicate") {
			return false, nil
		}
		return false, result.Error
	}
	return true, nil
}

// ReleaseBackchannelLogout 删除未完成的注销事件占位，使失败请求可以安全重试。
func (s *GORMOIDCStore) ReleaseBackchannelLogout(ctx context.Context, jti string) error {
	return s.db.WithContext(ctx).Exec("DELETE FROM con_oidc_backchannel_logout_replay WHERE jti_hash = ?", backchannelLogoutJTIHash(jti)).Error
}

// RevokeSessionsForSubject 按 OIDC subject 撤销当前租户下所有会话；subject 不匹配时不会扩大撤销范围。
func (s *GORMOIDCStore) RevokeSessionsForSubject(ctx context.Context, tenantID, subject string, now time.Time) error {
	return s.db.WithContext(ctx).Model(&gormSession{}).
		Where("tenant_id = ? AND revoked_at IS NULL AND JSON_UNQUOTE(JSON_EXTRACT(principal_json, '$.subject')) = ?", tenantID, subject).
		Updates(map[string]any{"revoked_at": now, "updated_at": now}).Error
}

func (s *GORMOIDCStore) DeleteExpired(ctx context.Context, now time.Time) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("expires_at <= ?", now.Add(-24*time.Hour)).Delete(&gormLoginTransaction{}).Error; err != nil {
			return err
		}
		return tx.Where("session_expires_at <= ? OR (revoked_at IS NOT NULL AND revoked_at <= ?)", now.Add(-24*time.Hour), now.Add(-24*time.Hour)).Delete(&gormSession{}).Error
	})
}

func sessionToGORM(record SessionRecord) gormSession {
	return gormSession{SessionIDHash: record.SessionIDHash, TenantID: record.TenantID, IdentityID: record.IdentityID,
		PersonID: nullableString(record.PersonID), PrincipalJSON: record.PrincipalJSON,
		AccessTokenCiphertext: record.AccessTokenCiphertext, RefreshTokenCiphertext: record.RefreshTokenCiphertext,
		IDTokenCiphertext: record.IDTokenCiphertext, AuthorizationRevision: record.AuthorizationRevision,
		AuthorizationCheckedAt: record.AuthorizationCheckedAt, TokenExpiresAt: record.TokenExpiresAt,
		SessionExpiresAt: record.SessionExpiresAt, LastSeenAt: record.LastSeenAt, LastInteractiveAt: record.LastInteractiveAt,
		RevokedAt: record.RevokedAt, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt}
}

func sessionFromGORM(row gormSession) SessionRecord {
	personID := ""
	if row.PersonID != nil {
		personID = *row.PersonID
	}
	return SessionRecord{SessionIDHash: row.SessionIDHash, TenantID: row.TenantID, IdentityID: row.IdentityID,
		PersonID: personID, PrincipalJSON: row.PrincipalJSON, AccessTokenCiphertext: row.AccessTokenCiphertext,
		RefreshTokenCiphertext: row.RefreshTokenCiphertext, IDTokenCiphertext: row.IDTokenCiphertext,
		AuthorizationRevision: row.AuthorizationRevision, AuthorizationCheckedAt: row.AuthorizationCheckedAt,
		TokenExpiresAt: row.TokenExpiresAt, SessionExpiresAt: row.SessionExpiresAt,
		LastSeenAt: row.LastSeenAt, LastInteractiveAt: row.LastInteractiveAt, RevokedAt: row.RevokedAt,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func nullableString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

type secretCodec struct{ aead cipher.AEAD }

func newSecretCodec(key []byte) (*secretCodec, error) {
	if len(key) != 32 {
		return nil, errors.New("OIDC session encryption key must contain exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &secretCodec{aead: aead}, nil
}

func (c *secretCodec) encrypt(plaintext string) ([]byte, error) {
	if plaintext == "" {
		return nil, nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

func (c *secretCodec) decrypt(ciphertext []byte) (string, error) {
	if len(ciphertext) == 0 {
		return "", nil
	}
	nonceSize := c.aead.NonceSize()
	if len(ciphertext) <= nonceSize {
		return "", errors.New("encrypted OIDC value is malformed")
	}
	plaintext, err := c.aead.Open(nil, ciphertext[:nonceSize], ciphertext[nonceSize:], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt OIDC value: %w", err)
	}
	return string(plaintext), nil
}

func tokenHash(value string) []byte {
	digest := sha256.Sum256([]byte(value))
	return digest[:]
}
