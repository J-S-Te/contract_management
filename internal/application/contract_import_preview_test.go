package application

import (
	"context"
	"errors"
	"github.com/j-s-te/contract-management/internal/contractdocument"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/testfixture"
	"io"
	"testing"
	"time"
)

type importPreviewRepository struct {
	recordingRepository
	batch contract.ImportBatch
}

// CreateImportBatch 保存测试台账。
// 参数：ctx为上下文，b为批次。
// 返回值：无错误。
func (r *importPreviewRepository) CreateImportBatch(ctx context.Context, b contract.ImportBatch) error {
	r.batch = b
	return nil
}

// GetImportBatch 返回测试批次。
// 参数：ctx、tenant、id为调用身份。
// 返回值：批次及无错误。
func (r *importPreviewRepository) GetImportBatch(ctx context.Context, tenant, id string) (contract.ImportBatch, error) {
	return r.batch, nil
}

// UpdateImportBatch 执行测试元数据回调。
// 参数：ctx、tenant、id为调用身份，change为待执行修改。
// 返回值：回调错误。
func (r *importPreviewRepository) UpdateImportBatch(ctx context.Context, tenant, id string, change func(*contract.ImportBatch) error) error {
	err := change(&r.batch)
	if err == nil {
		r.batch.Version++
	}
	return err
}

// ClaimImportBatch 使预检测试禁止后台业务执行。
// 参数：ctx及now为调度条件。
// 返回值：没有任务。
func (r *importPreviewRepository) ClaimImportBatch(ctx context.Context, now time.Time) (contract.ImportBatch, bool, error) {
	return contract.ImportBatch{}, false, nil
}

// CommitImportDraft 禁止预检测试中的合同创建副作用。
// 参数：ctx、tenant、batch、revision、epoch、item、c和actor为创建参数。
// 返回值：若误调用则返回错误。
func (r *importPreviewRepository) CommitImportDraft(ctx context.Context, tenant, batch string, revision, epoch uint64, item string, c contract.Contract, actor string) (string, error) {
	return "", errors.New("preview attempted creation")
}

type importPreviewGateway struct {
	body   []byte
	writes int
}

// Upload 记录不应在预检调用的上传。
// 参数：ctx及其余参数为上传身份和流。
// 返回值：固定测试对象。
func (g *importPreviewGateway) Upload(ctx context.Context, a, b, c, d, e string, body io.Reader) (string, error) {
	g.writes++
	return "file", nil
}

// Bind 记录不应在预检调用的绑定。
// 参数：ctx及其余参数为绑定身份。
// 返回值：无错误。
func (g *importPreviewGateway) Bind(ctx context.Context, a, b, c, d, e, f string) error {
	g.writes++
	return nil
}

// Download 返回预检原件。
// 参数：ctx及id为读取范围。
// 返回值：测试原件。
func (g *importPreviewGateway) Download(ctx context.Context, id string) ([]byte, error) {
	return g.body, nil
}

// TestImportPreviewNoBusinessSideEffects 验证预检复用字段/目录规则但不创建业务对象。
// 参数：t为测试上下文。
// 返回值：无；任何写副作用、旧预检或越权读取均失败。
func TestImportPreviewNoBusinessSideEffects(t *testing.T) {
	ctx := context.Background()
	pdf := testfixture.PDF("original contract")
	source, e := contractdocument.ValidateExternalSource("one.pdf", "application/pdf", pdf)
	if e != nil {
		t.Fatal(e)
	}
	repository := &importPreviewRepository{}
	gateway := &importPreviewGateway{body: pdf}
	actor := Principal{TenantID: "tenant", UserID: "actor", IdentityID: "actor", Permissions: map[string]bool{"contract.create": true, "contract.import.preview": true, "contract.import.commit": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.create": {AllowAll: true}, "contract.import.preview": {AllowAll: true}, "contract.import.commit": {AllowAll: true}}}
	repository.batch = contract.ImportBatch{ID: "batch", TenantID: actor.TenantID, ActorID: actor.UserID, Revision: 2, Version: 4, State: "DRAFT", SourceSystem: "legacy", ExpiresAt: time.Now().Add(time.Hour), Sources: []contract.ImportSource{{ID: "source", ItemKey: "one", Filename: "one.pdf", MediaType: "application/pdf", SHA256: source.SHA256, GatewayID: "object", State: "READY"}}, Items: []contract.ImportItemState{{ID: "item", Item: contract.ImportItem{Key: "one", ExternalRecordID: "old-1", Mode: contract.ImportNormal, SourceFileRef: "source", Metadata: contract.ImportMetadata{Title: "导入测试", ContractType: "直签", CRMCustomerID: 7, CustomerName: "历史客户名", AmountMinor: 10000, Currency: "CNY", ServiceItems: []contract.ServiceItem{{ServiceType: "软件测试", Site: "北京", Batch: "第一批", Category: "软件测试", TestMode: "STANDARD"}}}}}}}
	s := &Service{Repo: repository, StampedFileGateway: gateway, ExternalPDFEnabled: true, ContractImportEnabled: true, CRMContractReferences: validCRMReference(7), DetectionCategories: enabledDetectionCategories("软件测试")}
	result, e := s.PreviewImport(ctx, actor, "batch", 2)
	if e != nil {
		t.Fatal(e)
	}
	if result.State != "PREVIEWED" || result.PreviewToken == "" || result.Items[0].Digest == "" {
		t.Fatalf("预检失败: %#v", result.Items[0].Issues)
	}
	if repository.created.ID != "" || gateway.writes != 0 {
		t.Fatal("预检产生业务创建或文件写入副作用")
	}
	if _, e = s.PreviewImport(ctx, actor, "batch", 1); !errors.Is(e, ErrImportConflict) {
		t.Fatal("旧revision未拒绝")
	}
	other := actor
	other.TenantID = "other"
	if _, e = s.GetImportBatch(ctx, other, "batch"); !errors.Is(e, ErrForbidden) {
		t.Fatal("跨租户被接受")
	}
	// 无效字段一次给出多个精确路径，不把空值补造成业务事实。
	invalid := contract.ImportItem{Mode: contract.ImportNormal}
	issues := importFieldIssues(invalid)
	fields := map[string]bool{}
	for _, issue := range issues {
		fields[issue.Field] = true
	}
	if !fields["metadata.title"] || !fields["metadata.service_items"] || !fields["metadata.crm_customer_id"] {
		t.Fatal("字段错误缺少定位")
	}
}

// TestImportRevisionPreservesCreatedBaseline 验证部分成功后只修未建单项，已建基线转更正状态。
// 参数：t为应用事务边界测试上下文。
// 返回值：无；旧映射被覆盖或旧预检仍可用时失败。
func TestImportRevisionPreservesCreatedBaseline(t *testing.T) {
	actor := Principal{TenantID: "tenant", UserID: "actor", IdentityID: "actor", Permissions: map[string]bool{"contract.import.preview": true}, PermissionScopes: allowAllScope("contract.import.preview")}
	first := contract.ImportItem{Key: "one", ExternalRecordID: "old-1", Mode: contract.ImportNormal, SourceFileRef: "file-1", Metadata: contract.ImportMetadata{Title: "created"}}
	second := contract.ImportItem{Key: "two", ExternalRecordID: "old-2", Mode: contract.ImportNormal, SourceFileRef: "file-2", Metadata: contract.ImportMetadata{Title: "pending"}}
	repo := &importPreviewRepository{batch: contract.ImportBatch{ID: "batch", TenantID: "tenant", ActorID: "actor", State: "PAUSED", Revision: 1, CommittedRevision: 1, Version: 2, OperationID: "operation", SourceSystem: "legacy", PreviewToken: "old-token", ExpiresAt: time.Now().Add(time.Hour), Sources: []contract.ImportSource{{ID: "file-1", ItemKey: "one", State: "READY"}, {ID: "file-2", ItemKey: "two", State: "READY"}}, Items: []contract.ImportItemState{{ID: "item-1", Item: first, ContractID: "contract-1", State: "COMPLETE"}, {ID: "item-2", Item: second, State: "BLOCKED"}}}}
	service := &Service{Repo: repo, ContractImportEnabled: true, ContractImportMaxItems: 100}
	second.Metadata.Title = "corrected"
	manifest := contract.ImportManifest{SchemaVersion: 1, SourceSystem: "legacy", Items: []contract.ImportItem{first, second}}
	if err := service.SetImportManifest(context.Background(), actor, "batch", 2, manifest); err != nil {
		t.Fatal(err)
	}
	if repo.batch.Revision != 2 || repo.batch.PreviewToken != "" || repo.batch.Items[0].ContractID != "contract-1" || repo.batch.Items[1].ID != "item-2" || repo.batch.Items[1].Item.Metadata.Title != "corrected" {
		t.Fatal("修订未保持已创建基线及稳定单项身份")
	}
	manifest.Items[0].Metadata.Title = "overwrite"
	err := service.SetImportManifest(context.Background(), actor, "batch", repo.batch.Version, manifest)
	if !errors.Is(err, ErrImportCorrectionRequired) || repo.batch.Items[0].Item.Metadata.Title != "created" || repo.batch.Items[0].State != "CORRECTION_REQUIRED" {
		t.Fatalf("已建单更正未隔离: error=%v state=%s", err, repo.batch.Items[0].State)
	}
}

// TestImportCorrectionCannotResumeCommittedRevision 验证同代次更正不会被恢复清除。
// 参数：t为测试上下文。
// 返回值：无；恢复清除更正标记或后台继续执行时失败。
func TestImportCorrectionCannotResumeCommittedRevision(t *testing.T) {
	actor := Principal{TenantID: "tenant", UserID: "actor", Permissions: map[string]bool{"contract.import.commit": true}, PermissionScopes: allowAllScope("contract.import.commit")}
	for _, state := range []string{"CORRECTION_REQUIRED", "NEEDS_REVIEW"} {
		repo := &importPreviewRepository{batch: contract.ImportBatch{ID: "batch", TenantID: "tenant", ActorID: "actor", Version: 2, Revision: 1, CommittedRevision: 1, OperationID: "op", State: "PARTIAL", Items: []contract.ImportItemState{{ID: "item", State: state, ContractID: "contract"}}}}
		svc := &Service{Repo: repo}
		err := svc.ControlImport(context.Background(), actor, "batch", 2, true, make([]byte, 32))
		if !errors.Is(err, ErrImportNeedsReview) {
			t.Fatalf("%s resume accepted: %v", state, err)
		}
	}
}
