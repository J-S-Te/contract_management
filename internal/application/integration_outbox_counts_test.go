package application

import (
	"context"
	"errors"
	"testing"
)

// integrationOutboxCountRepoStub 只实现 readyz 所需的死信计数能力，
// ListFailedIntegrationEvents / RetryFailedIntegrationEvent 由嵌入接口占位。
type integrationOutboxCountRepoStub struct {
	Repository
	counts map[string]int64
	err    error
	calls  int
}

func (s *integrationOutboxCountRepoStub) CountDeadIntegrationOutboxEvents(context.Context) (map[string]int64, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.counts, nil
}

// BIZ-XEVT2-03：服务层把四类边的 dead 计数固定补齐后交给 /readyz。
func TestServiceIntegrationOutboxDeadCountsNormalizesKinds(t *testing.T) {
	repository := &integrationOutboxCountRepoStub{counts: map[string]int64{
		IntegrationOutboxFinancial: 2,
		IntegrationOutboxProject:   1,
	}}
	counts, err := (&Service{Repo: repository}).IntegrationOutboxDeadCounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if repository.calls != 1 {
		t.Fatalf("repository calls = %d, want 1", repository.calls)
	}
	want := map[string]int64{
		IntegrationOutboxFinancial:      2,
		IntegrationOutboxResponsibility: 0,
		IntegrationOutboxProject:        1,
		IntegrationOutboxLink:           0,
	}
	if len(counts) != len(want) {
		t.Fatalf("counts = %+v, want %d kinds", counts, len(want))
	}
	for kind, value := range want {
		if counts[kind] != value {
			t.Fatalf("%s dead = %d, want %d; counts = %+v", kind, counts[kind], value, counts)
		}
	}
}

// 查询失败必须原样上抛，调用方不能把错误变成 0 条死信。
func TestServiceIntegrationOutboxDeadCountsPropagatesQueryError(t *testing.T) {
	boom := errors.New("mysql: connection refused")
	_, err := (&Service{Repo: &integrationOutboxCountRepoStub{err: boom}}).IntegrationOutboxDeadCounts(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

// 未装配仓储或不具备计数能力时返回 unsupported，由 /readyz 标 disabled。
func TestServiceIntegrationOutboxDeadCountsRequiresCounterCapability(t *testing.T) {
	for name, service := range map[string]*Service{
		"nil-service":      nil,
		"nil-repo":         {},
		"non-counter-repo": {Repo: &integrationOutboxRepoStub{}},
	} {
		if _, err := service.IntegrationOutboxDeadCounts(context.Background()); !errors.Is(err, ErrIntegrationOutboxCountsUnsupported) {
			t.Fatalf("%s err = %v, want ErrIntegrationOutboxCountsUnsupported", name, err)
		}
	}
}
