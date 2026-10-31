package httpapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/J-S-Te/license-core"
	driver "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	store "github.com/j-s-te/contract-management/internal/infrastructure/mysql"
	"github.com/j-s-te/contract-management/internal/migration"
	"github.com/j-s-te/contract-management/internal/testfixture"
	"github.com/j-s-te/contract-management/internal/transport/httpapi"
	"github.com/j-s-te/contract-management/migrations"
	"github.com/oklog/ulid/v2"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 本文件仅验证隔离环境的真实 TCP、Router、应用服务和 MySQL 事务。
// 身份、许可、CRM/类别目录及文件网关是显式测试替身，不证明现场 OIDC 或外部集成。
type networkImportIdentity struct {
	actor application.Principal
	hash  [32]byte
}

// Authenticate 提供仅限本隔离服务器的测试主体，不接收或伪造业务会话。
// 参数：ctx为请求上下文，request为真实TCP收到的测试请求。
// 返回值：固定测试主体；缺少测试标记时拒绝。
func (n networkImportIdentity) Authenticate(ctx context.Context, request *http.Request) (application.Principal, error) {
	if request.Header.Get("X-Isolated-Network-Test") != "contract-import" {
		return application.Principal{}, application.ErrForbidden
	}
	return n.actor, nil
}

// ImportExecutionSession 返回外部身份替身专用的引用，不构造任何生产会话。
// 参数：request为已经通过测试身份验证的请求。
// 返回值：测试会话引用；无错误。
func (n networkImportIdentity) ImportExecutionSession(request *http.Request) ([]byte, error) {
	return append([]byte(nil), n.hash[:]...), nil
}

type networkImportLicense struct{}

// Check 显式替代隔离测试的外部商业许可检查。
// 参数：ctx为上下文，operation为真实应用申请的许可操作。
// 返回值：测试允许；不证明部署许可有效。
func (networkImportLicense) Check(ctx context.Context, operation core.Operation) error { return nil }

type networkImportDirectory struct{}

// Resolve 只提供合成客户，不调用业务CRM或接受真实客户标识。
// 参数：ctx为上下文，customer为合成客户，opportunity为空，actor为测试操作者。
// 返回值：客户7的目录引用；其它查询拒绝。
func (networkImportDirectory) Resolve(ctx context.Context, customer uint64, opportunity, actor string) (application.CRMContractReference, error) {
	if customer != 7 || opportunity != "" || actor == "" {
		return application.CRMContractReference{}, application.ErrCRMReferenceInvalid
	}
	return application.CRMContractReference{Customer: application.CRMCustomerReference{
		ID: 7, Name: "隔离网络测试客户", Status: "ACTIVE",
	}}, nil
}

// List 提供合成合同允许的检测类别替身。
// 参数：ctx为请求上下文。
// 返回值：唯一启用类别；无错误。
func (networkImportDirectory) List(ctx context.Context) ([]application.DetectionCategory, error) {
	return []application.DetectionCategory{{Category: "软件测试", Enabled: true}}, nil
}

type networkImportGateway struct {
	mu       sync.Mutex
	objects  map[string][]byte
	bindings map[string]bool
}

// Upload 保存真实合成PDF字节，并按应用提供的稳定请求号恢复对象。
// 参数：ctx为上下文，request为请求身份，其余参数为文件元数据，body为原件。
// 返回值：测试对象身份；读取失败时返回错误。
func (g *networkImportGateway) Upload(ctx context.Context, request, app, kind, name, media string, body io.Reader) (string, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if prior, ok := g.objects[request]; ok && !bytes.Equal(prior, data) {
		return "", errors.New("test gateway request identity conflict")
	}
	g.objects[request] = append([]byte(nil), data...)
	return request, nil
}

// Bind 记录精确资源身份的文件绑定，不伪造网关HTTP或完成回执。
// 参数：ctx为上下文，app/file/resource/id/kind/name为应用的实际绑定信息。
// 返回值：对象存在时成功，否则返回错误。
func (g *networkImportGateway) Bind(ctx context.Context, app, file, resource, id, kind, name string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.objects[file]; !ok {
		return errors.New("test gateway object missing")
	}
	g.bindings[strings.Join([]string{app, file, resource, id, kind}, "/")] = true
	return nil
}

// Download 返回隔离网关实际保存的原件字节副本。
// 参数：ctx为上下文，id为上传返回的对象身份。
// 返回值：原件或不存在错误。
func (g *networkImportGateway) Download(ctx context.Context, id string) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	data, ok := g.objects[id]
	if !ok {
		return nil, errors.New("test gateway object missing")
	}
	return append([]byte(nil), data...), nil
}

// networkImportDatabase 只迁移显式配置的独立回环测试库。
// 参数：t为测试上下文；CONTRACT_TEST_DSN必须是contract_test_命名的回环TCP库。
// 返回值：真实数据库连接；未配置时跳过，地址或命名不合法时失败。
func networkImportDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	raw := os.Getenv("CONTRACT_TEST_DSN")
	if raw == "" {
		t.Skip("explicit CONTRACT_TEST_DSN required; no database connection attempted")
	}
	cfg, err := driver.ParseDSN(raw)
	if err != nil {
		t.Fatal("invalid isolated test DSN")
	}
	host, _, addressErr := net.SplitHostPort(cfg.Addr)
	if cfg.Net != "tcp" || addressErr != nil || host != "127.0.0.1" ||
		!strings.HasPrefix(cfg.DBName, "contract_test_") {
		t.Fatal("isolated loopback TCP contract_test_* schema required")
	}
	cfg.ParseTime = true
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := migration.Run(ctx, cfg.FormatDSN(), migrations.Files); err != nil {
		t.Fatalf("isolated migrations failed: %v", err)
	}
	db, err := gorm.Open(gormmysql.Open(cfg.FormatDSN()), &gorm.Config{
		TranslateError: true, Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal("isolated database open failed")
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal("isolated database pool failed")
	}
	pool.SetMaxOpenConns(4)
	t.Cleanup(func() { pool.Close() })
	return db
}

type networkImportClient struct {
	url    string
	client *http.Client
}

// request 通过真实TCP发送请求，丢弃模式只检查状态而不解析响应载荷。
// 参数：method/path为路由，body/type为请求，key为幂等键，discard控制主动不用响应。
// 返回值：HTTP状态、已读取响应及传输错误；丢弃模式载荷为空。
func (n networkImportClient) request(method, path string, body []byte, contentType, key string, discard bool) (int, []byte, error) {
	req, err := http.NewRequest(method, n.url+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Origin", n.url)
	req.Header.Set("X-Isolated-Network-Test", "contract-import")
	req.Header.Set("Content-Type", contentType)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	response, err := n.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	if discard {
		_, err = io.Copy(io.Discard, response.Body)
		return response.StatusCode, nil, err
	}
	data, err := io.ReadAll(response.Body)
	return response.StatusCode, data, err
}

// networkImportJSON 执行正常HTTP动作并解析权威业务载荷。
// 参数：t为上下文，n为TCP客户端，method/path为路由，payload为DTO，key为幂等键，want为预期状态。
// 返回值：解析后的台账或文件；任何状态和信封错误均使测试失败。
func networkImportJSON[T any](t *testing.T, n networkImportClient, method, path string, payload any, key string, want int) T {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	status, data, err := n.request(method, path, body, "application/json", key, false)
	if err != nil || status != want {
		t.Fatalf("%s %s: status=%d expected=%d err=%v response=%s", method, path, status, want, err, data)
	}
	var reply struct {
		Code string `json:"code"`
		Data T      `json:"data"`
	}
	if json.Unmarshal(data, &reply) != nil || reply.Code != "OK" {
		t.Fatalf("invalid response: %s", data)
	}
	return reply.Data
}

// networkPreparedImport 通过正式HTTP上传、清单保存及预检准备一个真实PDF单项。
// 参数：t为上下文，n为TCP客户端，b为当前空批次。
// 返回值：冻结预检台账及原件SHA256；本步骤不得创建合同。
func networkPreparedImport(t *testing.T, n networkImportClient, b contract.ImportBatch) (contract.ImportBatch, string) {
	t.Helper()
	pdf := testfixture.PDF("isolated network idempotency")
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	file, err := form.CreateFormFile("file", "network-original.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write(pdf); err != nil {
		t.Fatal(err)
	}
	if err = form.WriteField("item_key", "network-one"); err != nil {
		t.Fatal(err)
	}
	if err = form.WriteField("expected_version", strconv.FormatUint(b.Version, 10)); err != nil {
		t.Fatal(err)
	}
	if err = form.Close(); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/contract-import-batches/" + b.ID
	status, data, err := n.request(http.MethodPost, path+"/files", body.Bytes(), form.FormDataContentType(), "", false)
	if err != nil || status != http.StatusCreated {
		t.Fatalf("upload status=%d err=%v response=%s", status, err, data)
	}
	var upload struct {
		Data contract.ImportSource `json:"data"`
	}
	if json.Unmarshal(data, &upload) != nil || upload.Data.ID == "" {
		t.Fatal("upload did not return file identity")
	}
	b = networkImportJSON[contract.ImportBatch](t, n, http.MethodGet, path, nil, "", 200)
	manifest := contract.ImportManifest{SchemaVersion: 1, SourceSystem: b.SourceSystem,
		Items: []contract.ImportItem{{Key: "network-one", ExternalRecordID: "network-source-one",
			Mode: contract.ImportNormal, SourceFileRef: upload.Data.ID,
			Metadata: contract.ImportMetadata{Title: "隔离TCP重试合同", ContractType: "直签",
				CRMCustomerID: 7, CustomerName: "隔离网络测试客户", AmountMinor: 12345, Currency: "CNY",
				ServiceItems: []contract.ServiceItem{{ServiceType: "软件测试", Site: "隔离站点",
					Batch: "第一批", Category: "软件测试", TestMode: "STANDARD"}}}}}}
	b = networkImportJSON[contract.ImportBatch](t, n, http.MethodPut, path+"/manifest",
		map[string]any{"expected_version": b.Version, "manifest": manifest}, "", 200)
	b = networkImportJSON[contract.ImportBatch](t, n, http.MethodPost, path+"/preview",
		map[string]any{"revision": b.Revision}, "", 200)
	if b.State != "PREVIEWED" || b.PreviewToken == "" || b.OperationID != "" {
		t.Fatalf("preview state=%s operation=%s items=%+v", b.State, b.OperationID, b.Items)
	}
	sum := sha256.Sum256(pdf)
	return b, hex.EncodeToString(sum[:])
}

// TestContractImportTCPMySQLIdempotency 验证真实TCP双请求、主动不用成功响应及持久业务恢复。
// 参数：t为上下文；仅运行于显式独立的回环MySQL测试库，外部系统使用声明的替身。
// 返回值：无；重复操作、来源或合同，异键未拒绝，或实际执行失败均使测试失败。
func TestContractImportTCPMySQLIdempotency(t *testing.T) {
	db := networkImportDatabase(t)
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "false")
	tenant, actorID := ulid.Make().String(), ulid.Make().String()
	actor := application.Principal{TenantID: tenant, UserID: actorID, IdentityID: actorID,
		Permissions: map[string]bool{}, PermissionScopes: map[string]contract.ScopeFilter{}}
	for _, permission := range []string{"contract.create", "contract.import.preview", "contract.import.commit"} {
		actor.Permissions[permission] = true
		actor.PermissionScopes[permission] = contract.ScopeFilter{AllowAll: true}
	}
	identity := networkImportIdentity{actor: actor, hash: sha256.Sum256([]byte(ulid.Make().String()))}
	gateway := &networkImportGateway{objects: map[string][]byte{}, bindings: map[string]bool{}}
	repo := store.NewRepository(db)
	service := &application.Service{Repo: repo, LicenseGate: networkImportLicense{},
		StampedFileGateway: gateway, StampedFileApplicationID: "contract",
		ExternalPDFEnabled: true, ContractImportEnabled: true, ContractImportMaxItems: 3,
		CRMContractReferences: networkImportDirectory{}, DetectionCategories: networkImportDirectory{}}
	service.ImportActorResolver = func(ctx context.Context, hash []byte, tenantID, userID string) (application.Principal, error) {
		if tenantID != tenant || userID != actorID || !bytes.Equal(hash, identity.hash[:]) {
			return application.Principal{}, application.ErrForbidden
		}
		return actor, nil
	}
	server := httptest.NewUnstartedServer(nil)
	t.Setenv("APP_PUBLIC_URL", "http://"+server.Listener.Addr().String())
	server.Config.Handler = httpapi.NewRouter(service, identity, nil)
	server.Start()
	t.Cleanup(server.Close)
	n := networkImportClient{url: server.URL, client: &http.Client{Timeout: 20 * time.Second}}
	create := map[string]any{"schema_version": 1, "source_system": "isolated-network"}
	first := networkImportJSON[contract.ImportBatch](t, n, "POST", "/api/v1/contract-import-batches", create, "same-create-key", 201)
	second := networkImportJSON[contract.ImportBatch](t, n, "POST", "/api/v1/contract-import-batches", create, "same-create-key", 201)
	if first.ID == second.ID {
		t.Fatal("create semantics changed: current endpoint does not support idempotency")
	}
	var count int64
	if err := db.Table("con_contract").Where("tenant_id=?", tenant).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("empty batch creation caused contracts: count=%d err=%v", count, err)
	}
	b, sha := networkPreparedImport(t, n, first)
	if err := db.Table("con_contract").Where("tenant_id=?", tenant).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("preview caused contracts: count=%d err=%v", count, err)
	}
	networkCommitAndComplete(t, n, service, repo, db, tenant, b, sha)
	t.Log("external substitutes: identity, license, file gateway, CRM and category directories; not onsite OIDC acceptance")
}

// networkCommitAndComplete 以两条并发TCP提交和一次同键恢复完成真实持久队列。
// 参数：t为上下文，n为客户端，service/repo/db为真实业务依赖，tenant/b/sha限定单份合成样本。
// 返回值：无；只允许一个operation、一个来源身份和一个实际合同。
func networkCommitAndComplete(t *testing.T, n networkImportClient, service *application.Service, repo *store.Repository, db *gorm.DB, tenant string, b contract.ImportBatch, sha string) {
	t.Helper()
	path := "/api/v1/contract-import-batches/" + b.ID
	payload := map[string]any{"revision": b.Revision, "preview_token": b.PreviewToken}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		status    int
		data      []byte
		err       error
		discarded bool
	}
	start, results := make(chan struct{}), make(chan result, 2)
	for index := range 2 {
		go func(discard bool) {
			<-start
			status, data, err := n.request("POST", path+"/commit", body, "application/json", "network-stable-commit", discard)
			results <- result{status, data, err, discard}
		}(index == 0)
	}
	close(start)
	operation := ""
	for range 2 {
		r := <-results
		if r.err != nil || r.status != 202 {
			t.Fatalf("concurrent status=%d err=%v body=%s", r.status, r.err, r.data)
		}
		if r.discarded {
			if len(r.data) != 0 {
				t.Fatal("discarded successful response was retained")
			}
			continue
		}
		var reply struct {
			Data contract.ImportBatch `json:"data"`
		}
		if json.Unmarshal(r.data, &reply) != nil || reply.Data.OperationID == "" {
			t.Fatal("concurrent accepted response missing operation")
		}
		operation = reply.Data.OperationID
	}
	recovered := networkImportJSON[contract.ImportBatch](t, n, "POST", path+"/commit", payload, "network-stable-commit", 202)
	if operation == "" || recovered.OperationID != operation {
		t.Fatal("same-key recovery changed stable operation")
	}
	status, data, err := n.request("POST", path+"/commit", body, "application/json", "network-different-key", false)
	if err != nil || status != 409 || !bytes.Contains(data, []byte("IMPORT_REVISION_CONFLICT")) {
		t.Fatalf("different key status=%d err=%v response=%s", status, err, data)
	}
	// 使用队列真实入口执行，不能直接写COMPLETE或返回固定成功。
	if err := service.DispatchImportOnce(context.Background()); err != nil {
		t.Fatalf("actual import dispatcher failed: %v", err)
	}
	final := networkImportJSON[contract.ImportBatch](t, n, "GET", path, nil, "", 200)
	if final.State != "COMPLETED" || final.OperationID != operation || len(final.Items) != 1 ||
		final.Items[0].State != "COMPLETE" || final.Items[0].ContractID == "" {
		t.Fatalf("incomplete actual operation: state=%s items=%+v", final.State, final.Items)
	}
	// 队列空闲重投不应再次创建任何业务实体。
	if err := service.DispatchImportOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"con_contract", "con_import_identity"} {
		var count int64
		if err := db.Table(table).Where("tenant_id=?", tenant).Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("%s count=%d err=%v", table, count, err)
		}
	}
	c, err := repo.GetContract(context.Background(), tenant, final.Items[0].ContractID)
	if err != nil || c.SourceSHA256 != sha || c.SourceFileStatus != "READY" ||
		c.AmountMinor != 12345 || c.Status != contract.StatusDraft {
		t.Fatalf("persisted contract invariant failed: err=%v sha=%s file=%s amount=%d status=%s", err, c.SourceSHA256, c.SourceFileStatus, c.AmountMinor, c.Status)
	}
	t.Logf("NETWORK_EVIDENCE concurrent_commit_requests=2 same_key_retry=1 discarded_success_response=1 different_key_conflict=409 operations=1 contracts=1 source_identities=1 source_sha256=%s", sha)
}
