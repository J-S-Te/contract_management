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
	RevokedAt              *time.Time
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

type OIDCStore interface {
	SaveLoginTransaction(context.Context, LoginTransactionRecord) error
	ConsumeLoginTransaction(context.Context, []byte, time.Time) (LoginTransactionRecord, error)
	CreateSession(context.Context, SessionRecord) error
	FindSession(context.Context, []byte, time.Time) (SessionRecord, error)
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
		SessionExpiresAt: record.SessionExpiresAt, RevokedAt: record.RevokedAt, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt}
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
		TokenExpiresAt: row.TokenExpiresAt, SessionExpiresAt: row.SessionExpiresAt, RevokedAt: row.RevokedAt,
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
