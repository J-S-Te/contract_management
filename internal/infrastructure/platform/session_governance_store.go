package platform

import (
	"context"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"time"
)

// AttachSessionLink 将票据绑定原始state，重复桥接不覆盖已经保存的票据。
// 参数：ctx为请求，hash为state摘要，cipher为票据密文，now为时钟。
// 返回值：仍未消费的原始登录事务；过期、重放或数据库故障返回错误。
func (s *GORMOIDCStore) AttachSessionLink(ctx context.Context, hash, cipher []byte, now time.Time) (LoginTransactionRecord, error) {
	var row gormLoginTransaction
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("state_hash = ? AND consumed_at IS NULL AND expires_at > ?", hash, now).Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrUnauthenticated
		}
		if err != nil {
			return err
		}
		if len(row.CentralTicketCiphertext) > 0 {
			return ErrUnauthenticated
		}
		return tx.Model(&gormLoginTransaction{}).Where("state_hash = ?", hash).Update("central_ticket_ciphertext", cipher).Error
	})
	if err != nil {
		return LoginTransactionRecord{}, err
	}
	return LoginTransactionRecord{StateHash: row.StateHash, TenantID: row.TenantID, NonceCiphertext: row.NonceCiphertext,
		CodeVerifierCiphertext: row.CodeVerifierCiphertext, ReturnPath: row.ReturnPath, ExpiresAt: row.ExpiresAt,
		CentralTicketCiphertext: cipher, ForceLogin: row.ForceLogin}, nil
}

// WithSessionLock 使用真实数据库行锁重读并轮换，防止不同实例并行重放旧refresh。
// 参数：ctx为请求，hash为Cookie摘要，now为时钟，update为有界网络刷新及快照核验。
// 返回值：最新持久记录；明确失效提交当前行撤销，其他错误回滚并返回依赖故障。
func (s *GORMOIDCStore) WithSessionLock(ctx context.Context, hash []byte, now time.Time, update sessionUpdateFunc) (SessionRecord, error) {
	var result SessionRecord
	var rejected error
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row gormSession
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("session_id_hash = ? AND revoked_at IS NULL AND session_expires_at > ?", hash, now).Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrUnauthenticated
		}
		if err != nil {
			return err
		}
		result = sessionFromGORM(row)
		updated, changed, err := update(result)
		if errors.Is(err, ErrUnauthenticated) || errors.Is(err, ErrAuthorizationForbidden) || errors.Is(err, ErrAuthorizationUnauthorized) || errors.Is(err, ErrAuthorizationInvalid) || refreshTokenWasRejected(err) {
			rejected = ErrUnauthenticated
			return tx.Model(&gormSession{}).Where("session_id_hash = ?", hash).Updates(map[string]any{"revoked_at": now, "updated_at": now}).Error
		}
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}
		result = updated
		return tx.Model(&gormSession{}).Where("session_id_hash = ?", hash).Updates(map[string]any{
			"person_id": nullableString(updated.PersonID), "principal_json": updated.PrincipalJSON,
			"access_token_ciphertext": updated.AccessTokenCiphertext, "refresh_token_ciphertext": updated.RefreshTokenCiphertext,
			"id_token_ciphertext": updated.IDTokenCiphertext, "authorization_revision": updated.AuthorizationRevision,
			"authorization_checked_at": updated.AuthorizationCheckedAt, "token_expires_at": updated.TokenExpiresAt,
			"updated_at": updated.UpdatedAt,
		}).Error
	})
	if err != nil {
		return SessionRecord{}, err
	}
	if rejected != nil {
		return SessionRecord{}, rejected
	}
	return result, nil
}

// ProcessScopedBackchannelLogout 按签名sid精确撤销并原子登记JTI，不扩大到其他浏览器。
// 参数：ctx为请求，tenant/subject/sid为已验证范围，jti为重放标识，now为时钟。
// 返回值：首次处理为true，重复投递为false；事务失败返回错误。
func (s *GORMOIDCStore) ProcessScopedBackchannelLogout(ctx context.Context, tenant, subject, sid, jti string, now time.Time) (bool, error) {
	if tenant == "" || sid == "" || jti == "" {
		return false, ErrUnauthenticated
	}
	claimed := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row := struct {
			JTIHash              []byte `gorm:"column:jti_hash"`
			ExpiresAt, CreatedAt time.Time
		}{backchannelLogoutJTIHash(jti), now.Add(5 * time.Minute), now}
		insert := tx.Table("con_oidc_backchannel_logout_replay").Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		if insert.Error != nil {
			return insert.Error
		}
		if insert.RowsAffected == 0 {
			return nil
		}
		claimed = true
		query := tx.Model(&gormSession{}).Where("tenant_id = ? AND oidc_sid = ? AND revoked_at IS NULL", tenant, sid)
		if subject != "" {
			query = query.Where("JSON_UNQUOTE(JSON_EXTRACT(principal_json,'$.subject')) = ?", subject)
		}
		return query.Updates(map[string]any{"revoked_at": now, "updated_at": now}).Error
	})
	return claimed, err
}
