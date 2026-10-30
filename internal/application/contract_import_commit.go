package application

import (
	"context"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/oklog/ulid/v2"
	"time"
)

// CommitImport 持久化异步操作身份，重复提交返回同一操作。
// 参数：ctx及actor为当前身份，id/revision/token冻结预检，key为幂等键，session为现有加密会话的哈希引用。
// 返回值：稳定操作台账或版本、权限及依赖错误。
func (s *Service) CommitImport(ctx context.Context, actor Principal, id string, revision uint64, token, key string, session []byte) (contract.ImportBatch, error) {
	r, e := s.importStore()
	if e != nil {
		return contract.ImportBatch{}, e
	}
	if len(key) < 8 || len(key) > 128 || len(session) != 32 || s.ImportActorResolver == nil {
		return contract.ImportBatch{}, ErrValidation
	}
	digest, _ := contract.ImportDigest(key)
	e = r.UpdateImportBatch(ctx, actor.TenantID, id, func(b *contract.ImportBatch) error {
		if e := importAccess(actor, *b, "contract.import.commit"); e != nil {
			return e
		}
		if actor.UserID != b.ActorID {
			return ErrForbidden
		}
		if b.OperationID != "" && b.CommittedRevision == revision {
			if b.Revision != revision || b.CommitKeyHash != digest {
				return ErrImportConflict
			}
			return nil
		}
		if !s.ContractImportEnabled && b.OperationID == "" {
			return ErrImportUnsupported
		}
		if b.Revision != revision || b.PreviewToken != token || token == "" || b.State != "PREVIEWED" || !b.PreviewExpiresAt.After(time.Now().UTC()) || !b.ExpiresAt.After(time.Now().UTC()) {
			return ErrImportConflict
		}
		for _, item := range b.Items {
			if item.Item.Mode != contract.ImportNormal {
				return ErrImportUnsupported
			}
		}
		b.CommitKeyHash, b.SessionHash = digest, append([]byte(nil), session...)
		if b.OperationID == "" {
			b.OperationID = ulid.Make().String()
		}
		b.CommittedRevision = b.Revision
		b.Epoch++
		b.State = "QUEUED"
		return nil
	})
	if e != nil {
		return contract.ImportBatch{}, e
	}
	return s.GetImportBatch(ctx, actor, id)
}

// ControlImport 暂停或恢复新认领，保留原操作及来源身份。
// 参数：ctx及actor为当前身份，id/version为台账版本，resume为恢复，session为恢复操作者会话。
// 返回值：权限、并发或存储错误。
func (s *Service) ControlImport(ctx context.Context, actor Principal, id string, version uint64, resume bool, session []byte) error {
	r, e := s.importStore()
	if e != nil {
		return e
	}
	return r.UpdateImportBatch(ctx, actor.TenantID, id, func(b *contract.ImportBatch) error {
		if e := importAccess(actor, *b, "contract.import.commit"); e != nil {
			return e
		}
		if b.Confirmation != nil && b.Confirmation.State != "STARTING" {
			return ErrImportConflict
		}
		if b.Version != version || b.OperationID == "" || b.State == "COMPLETED" {
			return ErrImportConflict
		}
		b.Epoch++
		b.LeaseUntil = time.Time{}
		if !resume {
			b.State = "PAUSED"
			return nil
		}
		for _, item := range b.Items {
			if item.State == "CORRECTION_REQUIRED" || item.State == "NEEDS_REVIEW" {
				return ErrImportNeedsReview
			}
		}
		if b.CommittedRevision != b.Revision {
			return ErrImportConflict
		}
		// 不允许管理员用自己的会话冒充原始创建人；重新登录后由原创建人恢复。
		if len(session) != 32 || actor.UserID != b.ActorID {
			return ErrForbidden
		}
		b.SessionHash = append([]byte(nil), session...)
		b.State = "QUEUED"
		for i := range b.Items {
			if b.Items[i].ContractID == "" {
				b.Items[i].Issues = nil
				b.Items[i].State = "READY"
			}
		}
		return nil
	})
}
