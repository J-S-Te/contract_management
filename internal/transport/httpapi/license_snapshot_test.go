package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/J-S-Te/license-core"
	runtime "github.com/J-S-Te/license-core/runtime"
	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"go.temporal.io/sdk/temporal"
)

type signedContractGate func(context.Context, core.Operation) error

func (f signedContractGate) Check(ctx context.Context, op core.Operation) error { return f(ctx, op) }

type contractLicenseHistoryRepo struct {
	application.Repository
	reads int
}

func (r *contractLicenseHistoryRepo) ListContracts(context.Context, string, string, string, string, int) ([]contract.Contract, error) {
	r.reads++
	return []contract.Contract{}, nil
}
func (r *contractLicenseHistoryRepo) GetContract(context.Context, string, string) (contract.Contract, error) {
	r.reads++
	return contract.Contract{ID: "existing", TenantID: "tenant", CreatedBy: "actor", OwnerIdentityID: "actor", Document: []byte("existing immutable document")}, nil
}

// 使用真实注册路由器、应用服务和活动，配合
// 独立签名的持久状态。认证及历史存储为
// 显式夹具；不声称完成 OIDC、MySQL 或 Temporal 服务验收。
func TestSignedMigrationExpiryContractHTTPServiceAndActivities(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("APP_PUBLIC_URL", "https://contract.example.test")
	t.Setenv("APP_CORS_ALLOWED_ORIGINS", "")
	for _, role := range []string{"admin", "sales"} {
		t.Run(role, func(t *testing.T) {
			pp, ps, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			vp, vs, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Unix(1800000000, 0).UTC()
			expiry := now.Add(time.Minute)
			binding := runtime.Binding{InstanceID: "test-instance", Environment: "production", Application: "contract_management", ServiceID: "contract-api"}
			path := filepath.Join(t.TempDir(), "state.json")
			reopen := func() *runtime.FileRuntime {
				t.Helper()
				r, err := runtime.NewFileRuntime(path, binding, map[string]ed25519.PublicKey{"platform": pp}, map[string]ed25519.PublicKey{"vendor": vp})
				if err != nil {
					t.Fatal(err)
				}
				return r
			}
			rt := reopen()
			license, err := core.Sign(core.License{ProtocolVersion: 1, Issuer: core.Issuer, ID: "license", Version: 1, CustomerID: "customer", ProductID: core.Product, Environment: binding.Environment, InstanceID: binding.InstanceID, IssuedAt: now.Unix() - 1000, NotBefore: now.Unix() - 100, Applications: []core.Application{{Code: binding.Application, NotBefore: now.Unix() - 100, ExpiresAt: expiry.Unix(), Kind: "FULL"}}}, "vendor", vs)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := runtime.SignSnapshot(runtime.PlatformSnapshot{Protocol: 1, InstanceID: binding.InstanceID, Environment: binding.Environment, Application: binding.Application, ServiceID: binding.ServiceID, Revision: 1, EnforcementState: runtime.Pending, MigrationEligible: true, CurrentLicenseJWS: license, HighestLicenseVersion: 1, SnapshotIssuedAt: now.Unix()}, "platform", ps)
			if err != nil {
				t.Fatal(err)
			}
			if err := rt.ApplySnapshot(context.Background(), snapshot, now); err != nil {
				t.Fatal(err)
			}
			workerBinding := binding
			workerBinding.ServiceID = "contract-worker"
			workerPath := filepath.Join(t.TempDir(), "worker-state.json")
			reopenWorker := func() *runtime.FileRuntime {
				t.Helper()
				worker, err := runtime.NewFileRuntime(workerPath, workerBinding, map[string]ed25519.PublicKey{"platform": pp}, map[string]ed25519.PublicKey{"vendor": vp})
				if err != nil {
					t.Fatal(err)
				}
				return worker
			}
			workerRuntime := reopenWorker()
			workerSnapshot, err := runtime.SignSnapshot(runtime.PlatformSnapshot{Protocol: 1, InstanceID: workerBinding.InstanceID, Environment: workerBinding.Environment, Application: workerBinding.Application, ServiceID: workerBinding.ServiceID, Revision: 1, EnforcementState: runtime.Pending, MigrationEligible: true, CurrentLicenseJWS: license, HighestLicenseVersion: 1, SnapshotIssuedAt: now.Unix()}, "platform", ps)
			if err != nil {
				t.Fatal(err)
			}
			if err := workerRuntime.ApplySnapshot(context.Background(), workerSnapshot, now); err != nil {
				t.Fatal(err)
			}
			gate := signedContractGate(func(ctx context.Context, op core.Operation) error { return rt.Evaluate(ctx, op, now) })
			workerGate := signedContractGate(func(ctx context.Context, op core.Operation) error { return workerRuntime.Evaluate(ctx, op, now) })
			if err := workerGate.Check(context.Background(), core.MUTATE_BUSINESS); err != nil {
				t.Fatalf("active independent Worker license rejected: %v", err)
			}
			repo := &contractLicenseHistoryRepo{}
			service := &application.Service{LicenseGate: gate, Repo: repo}
			actor := application.Principal{TenantID: "tenant", UserID: "actor", IdentityID: "actor", Roles: []string{role}, Permissions: map[string]bool{"contract.create": true, "contract.read": true, "approval.process": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.create": {AllowAll: true}, "contract.read": {AllowAll: true}, "approval.process": {AllowAll: true}}}
			router := NewRouter(service, identityFunc(func(context.Context, *http.Request) (application.Principal, error) { return actor, nil }), nil)
			request := func(method, path string) *httptest.ResponseRecorder {
				t.Helper()
				r := httptest.NewRequest(method, path, strings.NewReader("["))
				r.Header.Set("Origin", "https://contract.example.test")
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				router.ServeHTTP(w, r)
				return w
			}
			// 有效许可证下，无效 JSON 会进入校验，
			// 无需有效业务写入或修改存储。
			if w := request("POST", "/api/v1/contracts"); w.Code != 422 || strings.Contains(w.Body.String(), "COMMERCIAL_LICENSE_RESTRICTED") {
				t.Fatalf("active create: %d %s", w.Code, w.Body.String())
			}
			for _, stage := range []string{"boundary", "offline", "reopened"} {
				if stage == "boundary" {
					now = expiry
				} else {
					now = expiry.Add(time.Hour)
				}
				if stage == "reopened" {
					rt = reopen()
					workerRuntime = reopenWorker()
				}
				for _, path := range []string{"/api/v1/contracts", "/api/v1/contracts/existing/submit-approval", "/api/v1/approvals/existing/approve", "/api/v1/opportunity-intakes"} {
					if w := request("POST", path); w.Code != 403 || !strings.Contains(w.Body.String(), "COMMERCIAL_LICENSE_RESTRICTED") {
						t.Errorf("%s %s: %d %s", stage, path, w.Code, w.Body.String())
					}
				}
				for _, path := range []string{"/api/v1/contracts", "/api/v1/contracts/existing/export", "/api/v1/auth/me"} {
					if w := request("GET", path); w.Code != 200 {
						t.Errorf("%s history %s: %d %s", stage, path, w.Code, w.Body.String())
					}
				}
				// 过期后，这些直接应用命令绝不能触碰
				// nil 仓储或工作流客户端。
				isolated := &application.Service{LicenseGate: gate}
				for name, run := range map[string]func() error{
					"create": func() error {
						_, err := isolated.CreateContract(context.Background(), actor, contract.Contract{})
						return err
					},
					"submit": func() error {
						_, err := isolated.SubmitContractVersion(context.Background(), actor, "existing", true, 1)
						return err
					},
					"approve": func() error {
						_, err := isolated.Command(context.Background(), actor, "existing", workflows.ApprovalCommand{})
						return err
					},
					"machine intake": func() error {
						_, err := isolated.AcceptOpportunityIntake(context.Background(), application.OpportunityIntake{})
						return err
					},
				} {
					if err := run(); !errors.Is(err, application.ErrCommercialLicense) {
						t.Errorf("%s %s service: %v", stage, name, err)
					}
				}
				activities := &workflows.Activities{LicenseGate: workerGate}
				for _, run := range []func() error{
					func() error {
						return activities.StartApproval(context.Background(), workflows.StartApprovalActivityInput{})
					},
					func() error {
						return activities.RecordCommand(context.Background(), workflows.RecordCommandActivityInput{})
					},
					func() error {
						return activities.CompleteApproval(context.Background(), workflows.CompleteApprovalActivityInput{})
					},
					func() error {
						return activities.CreateNotification(context.Background(), workflows.NotifyActivityInput{})
					},
					func() error {
						_, err := activities.ArchiveExpired(context.Background(), workflows.ExpiredArchiveInput{})
						return err
					},
				} {
					var denial *temporal.ApplicationError
					if err := run(); !errors.As(err, &denial) || !denial.NonRetryable() || denial.Type() != "COMMERCIAL_LICENSE_RESTRICTED" {
						t.Errorf("%s Activity denial: %v", stage, err)
					}
				}
			}
			actor.Permissions = nil
			before := repo.reads
			for _, path := range []string{"/api/v1/contracts", "/api/v1/contracts/existing/export"} {
				if w := request("GET", path); w.Code != 403 {
					t.Errorf("history permission bypass: %s %d", path, w.Code)
				}
			}
			if repo.reads != before {
				t.Fatal("denied historical reads reached repository")
			}
		})
	}
}
