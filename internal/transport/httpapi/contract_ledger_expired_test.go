package httpapi

import (
	"testing"

	"github.com/j-s-te/contract-management/internal/application"
)

// TestListContractsForwardsExpiredParam 锁定协议层：expired 与 keyword/status/type
// 一样原样下推到服务层查询结构，响应形状不变（分页信封）。
func TestListContractsForwardsExpiredParam(t *testing.T) {
	c, recorder, repository := ledgerRequest(t, "/api/v1/contracts?page=1&page_size=20&expired=true&keyword=HT")
	h := &Handler{service: &application.Service{Repo: repository}}
	h.listContracts(c)

	if recorder.Code != 200 {
		t.Fatalf("status = %d; body = %s", recorder.Code, recorder.Body.String())
	}
	if repository.query.Expired != "true" || repository.query.Keyword != "HT" {
		t.Fatalf("筛选下推 = %#v，want expired=true keyword=HT", repository.query)
	}
	// 响应仍是分页信封（items/total/page/page_size），过期筛选不改变协议形状。
	body := decodeLedgerEnvelope(t, recorder.Body.Bytes())
	if len(body.Data) == 0 || body.Data[0] != '{' {
		t.Fatalf("分页请求的 data 应仍是对象信封: %s", body.Data)
	}
}

// TestListContractsKeepsLegacyShapeWithExpiredParam 锁定向后兼容：
// 带 expired 但不带 page/page_size 时仍返回裸数组（旧调用方不因新参数改变形状）。
func TestListContractsKeepsLegacyShapeWithExpiredParam(t *testing.T) {
	c, recorder, repository := ledgerRequest(t, "/api/v1/contracts?limit=50&expired=false")
	h := &Handler{service: &application.Service{Repo: repository}}
	h.listContracts(c)

	if recorder.Code != 200 {
		t.Fatalf("status = %d; body = %s", recorder.Code, recorder.Body.String())
	}
	body := decodeLedgerEnvelope(t, recorder.Body.Bytes())
	if body.Data[0] != '[' {
		t.Fatalf("旧协议 data 必须是裸数组: %s", body.Data)
	}
	if repository.query.Expired != "false" || repository.query.Limit != 50 {
		t.Fatalf("筛选下推 = %#v，want expired=false limit=50", repository.query)
	}
}

// TestListContractsWithoutExpiredParamSendsEmptyValue 锁定默认不启用过期筛选：
// 未带 expired 时下推空串，仓储据此不追加任何谓词（保持既有 SQL 形状）。
func TestListContractsWithoutExpiredParamSendsEmptyValue(t *testing.T) {
	c, recorder, repository := ledgerRequest(t, "/api/v1/contracts")
	h := &Handler{service: &application.Service{Repo: repository}}
	h.listContracts(c)

	if recorder.Code != 200 {
		t.Fatalf("status = %d; body = %s", recorder.Code, recorder.Body.String())
	}
	if repository.query.Expired != "" {
		t.Fatalf("未带 expired 时下推 = %q，want 空串", repository.query.Expired)
	}
}

// TestListContractsInvalidExpiredValueIsIgnoredNotRejected 锁定宽松口径：
// 非法 expired 取值不返回 422，仍按正常台账请求处理（由仓储退化为不过滤）。
func TestListContractsInvalidExpiredValueIsIgnoredNotRejected(t *testing.T) {
	c, recorder, repository := ledgerRequest(t, "/api/v1/contracts?expired=maybe&page=1")
	h := &Handler{service: &application.Service{Repo: repository}}
	h.listContracts(c)

	if recorder.Code != 200 {
		t.Fatalf("status = %d，非法 expired 不应报错; body = %s", recorder.Code, recorder.Body.String())
	}
	if repository.query.Expired != "maybe" {
		t.Fatalf("非法值应原样下推由仓储忽略，实际 = %q", repository.query.Expired)
	}
	if recorder.Body.Len() == 0 {
		t.Fatal("非法 expired 仍应返回台账数据")
	}
}
