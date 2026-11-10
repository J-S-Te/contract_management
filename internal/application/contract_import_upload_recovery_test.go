package application

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/contractdocument"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/testfixture"
)

type recoveryImportGateway struct {
	importPreviewGateway
	status                string
	resolveErr, uploadErr error
	uploaded, bound       []string
	onResolve             func()
}

// ResolveUploadRequest 模拟外部网关的权威请求状态。
// 参数：ctx为上下文，request为旧上传请求身份。
// 返回值：固定文件身份、所设状态和目录错误。
func (g *recoveryImportGateway) ResolveUploadRequest(ctx context.Context, request string) (string, string, error) {
	if g.onResolve != nil {
		g.onResolve()
	}
	return "old-file", g.status, g.resolveErr
}

// Upload 记录服务实际选择的上传代次。
// 参数：ctx及其余参数限定请求与原件流。
// 返回值：新网关文件ID或所设依赖错误。
func (g *recoveryImportGateway) Upload(ctx context.Context, request, app, purpose, name, media string, body io.Reader) (string, error) {
	g.uploaded = append(g.uploaded, request)
	return "new-file", g.uploadErr
}

// Bind 记录被绑定的真实网关文件身份。
// 参数：ctx及其余参数限定文件与业务资源。
// 返回值：无错误。
func (g *recoveryImportGateway) Bind(ctx context.Context, app, file, resource, id, binding, name string) error {
	g.bound = append(g.bound, file)
	return nil
}

// recoveryImportFixture 建立已暂存但上传回执未完成的隔离夹具。
// 参数：t为测试上下文，gateway为受控外部边界。
// 返回值：真实服务、批次仓库、权限主体和合法PDF。
func recoveryImportFixture(t *testing.T, gateway *recoveryImportGateway) (*Service, *importPreviewRepository, Principal, []byte) {
	t.Helper()
	body := testfixture.PDF("upload recovery fixture")
	validated, err := contractdocument.ValidateExternalSource("one.pdf", "application/pdf", body)
	if err != nil {
		t.Fatal(err)
	}
	actor := Principal{TenantID: "tenant", UserID: "actor", Permissions: map[string]bool{"contract.import.preview": true}, PermissionScopes: allowAllScope("contract.import.preview")}
	repo := &importPreviewRepository{batch: contract.ImportBatch{
		ID: "batch", TenantID: "tenant", ActorID: "actor", State: "DRAFT", Version: 1, Epoch: 3,
		ExpiresAt: time.Now().Add(time.Hour), Sources: []contract.ImportSource{{
			ID: "old-source", ItemKey: "one", Filename: "one.pdf", MediaType: "application/pdf", SHA256: validated.SHA256, State: "UPLOADING",
		}},
	}}
	service := &Service{Repo: repo, StampedFileGateway: gateway, ContractImportEnabled: true, ExternalPDFEnabled: true}
	return service, repo, actor, body
}

// TestImportUploadRecovery 避免终态失败永久409、回执丢失重复上传及未知状态擅自换代。
// 参数：t为不访问真实存储或业务库的测试上下文。
// 返回值：无；代次、绑定或失败保护不符时失败。
func TestImportUploadRecovery(t *testing.T) {
	for _, status := range []string{"FAILED", "READY", "NOT_FOUND", "WRITING", "REJECTED", "CANCELLED", "UNKNOWN", "UNAVAILABLE"} {
		t.Run(status, func(t *testing.T) {
			gateway := &recoveryImportGateway{status: status}
			if status == "UNAVAILABLE" {
				gateway.resolveErr = errors.New("directory unavailable")
			}
			service, repo, actor, body := recoveryImportFixture(t, gateway)
			source, err := service.UploadImportSource(context.Background(), actor, "batch", 1, "one", "one.pdf", "application/pdf", body)
			switch status {
			case "FAILED":
				if err != nil || source.State != "READY" || source.ID == "old-source" || len(repo.batch.Sources) != 2 || repo.batch.Sources[0].State != "FAILED" {
					t.Fatalf("终态失败未安全换代: source=%+v err=%v sources=%+v", source, err, repo.batch.Sources)
				}
				if len(gateway.uploaded) != 1 || gateway.uploaded[0] == "contract-import-old-source" {
					t.Fatal("仍复用失败幂等身份")
				}
			case "READY":
				if err != nil || source.ID != "old-source" || source.GatewayID != "old-file" || len(gateway.uploaded) != 0 || len(gateway.bound) != 1 || gateway.bound[0] != "old-file" {
					t.Fatalf("成功回执丢失未复用: %+v %v uploaded=%v bound=%v", source, err, gateway.uploaded, gateway.bound)
				}
			case "NOT_FOUND":
				if err != nil || source.ID != "old-source" || len(gateway.uploaded) != 1 || gateway.uploaded[0] != "contract-import-old-source" {
					t.Fatalf("未预留的请求必须保留原身份重试: source=%+v err=%v", source, err)
				}
			default:
				if err == nil || len(gateway.uploaded) != 0 || len(gateway.bound) != 0 || len(repo.batch.Sources) != 1 {
					t.Fatalf("不可确认状态发生新上传: status=%s err=%v uploaded=%v", status, err, gateway.uploaded)
				}
			}
		})
	}
}

// TestImportUploadStorageFailurePreservesAttempt 验证容量或网络失败不会丢弃尚未确认的上传代次。
// 参数：t为测试上下文。
// 返回值：无；依赖错误身份或保留状态不符时失败。
func TestImportUploadStorageFailurePreservesAttempt(t *testing.T) {
	gateway := &recoveryImportGateway{status: "NOT_FOUND", uploadErr: errors.New("file gateway returned HTTP 500")}
	service, repo, actor, body := recoveryImportFixture(t, gateway)
	repo.batch.Sources = nil
	_, err := service.UploadImportSource(context.Background(), actor, "batch", 1, "one", "one.pdf", "application/pdf", body)
	if !errors.Is(err, ErrImportSourceStorageUnavailable) || len(repo.batch.Sources) != 1 || repo.batch.Sources[0].State != "UPLOADING" || len(gateway.bound) != 0 {
		t.Fatalf("失败未保留可对账代次: err=%v sources=%+v", err, repo.batch.Sources)
	}
}

// TestImportUploadRecoveryFencesConcurrentRevision 验证网关查询期间批次改变不能继续换代或绑定。
// 参数：t为测试上下文。
// 返回值：无；版本围栏失效时失败。
func TestImportUploadRecoveryFencesConcurrentRevision(t *testing.T) {
	for _, status := range []string{"FAILED", "READY"} {
		t.Run(status, func(t *testing.T) {
			gateway := &recoveryImportGateway{status: status}
			service, repo, actor, body := recoveryImportFixture(t, gateway)
			gateway.onResolve = func() { repo.batch.Epoch++; repo.batch.State = "QUEUED" }
			_, err := service.UploadImportSource(context.Background(), actor, "batch", 1, "one", "one.pdf", "application/pdf", body)
			if !errors.Is(err, ErrImportConflict) || len(gateway.uploaded) != 0 || len(gateway.bound) != 0 || len(repo.batch.Sources) != 1 {
				t.Fatalf("迟到恢复越过批次围栏: err=%v uploaded=%v bound=%v", err, gateway.uploaded, gateway.bound)
			}
		})
	}
}
