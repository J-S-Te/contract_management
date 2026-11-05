package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

type localCatalogTargetDirectory struct {
	client *PersonnelDirectoryClient
	tenant string
}
type catalogDirectoryUser struct {
	ID            string `json:"user_id"`
	Name          string `json:"display_name"`
	Organizations []struct {
		ID   string `json:"organization_id"`
		Name string `json:"organization_name"`
	} `json:"organizations"`
}

// NewLocalCatalogTargetDirectory 复用既有最小owner_directory.read机器边界并绑定运行租户。
// 参数：directory为已有人员客户端，tenant为可信OIDC运行租户。
// 返回值：目标与会员目录；未配置可信客户端时返回nil并由写操作失败关闭。
func NewLocalCatalogTargetDirectory(directory application.PersonnelDirectory, tenant string) application.LocalCatalogTargetDirectory {
	client, ok := directory.(*PersonnelDirectoryClient)
	if !ok || client == nil || strings.TrimSpace(tenant) == "" {
		return nil
	}
	return &localCatalogTargetDirectory{client: client, tenant: strings.TrimSpace(tenant)}
}

// query 通过可信租户绑定的机器目录读取有效成员，不接受请求覆盖租户。
// 参数：ctx/actor为请求主体，query为固定销售或本人过滤及分页。
// 返回值：平台当前有效用户页及总量，任何协议或依赖错误失败关闭。
func (d *localCatalogTargetDirectory) query(ctx context.Context, actor application.Principal, query url.Values) ([]catalogDirectoryUser, int64, error) {
	if actor.TenantID != d.tenant || actor.TenantID == "" {
		return nil, 0, application.ErrForbidden
	}
	token, err := d.client.accessToken(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: token request failed", application.ErrPersonnelDirectoryUnavailable)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.client.baseURL+"/api/v1/internal/owner-directory?"+query.Encode(), nil)
	if err != nil {
		return nil, 0, application.ErrPersonnelDirectoryUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	response, err := d.client.client.Do(req)
	if err != nil {
		return nil, 0, application.ErrPersonnelDirectoryUnavailable
	}
	defer response.Body.Close()
	var envelope struct {
		Data *struct {
			Items []catalogDirectoryUser `json:"items"`
			Total int64                  `json:"total"`
		} `json:"data"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&envelope) != nil {
		return nil, 0, application.ErrPersonnelDirectoryUnavailable
	}
	if envelope.Data == nil || envelope.Data.Items == nil || envelope.Data.Total < int64(len(envelope.Data.Items)) {
		return nil, 0, application.ErrPersonnelDirectoryUnavailable
	}
	for _, user := range envelope.Data.Items {
		if user.ID == "" || user.Name == "" || len(user.ID) > 128 {
			return nil, 0, application.ErrPersonnelDirectoryUnavailable
		}
		for _, org := range user.Organizations {
			if org.ID == "" || org.Name == "" || len(org.ID) > 64 {
				return nil, 0, application.ErrPersonnelDirectoryUnavailable
			}
		}
	}
	return envelope.Data.Items, envelope.Data.Total, nil
}

// ListCatalogTargets 返回当前有效销售和其有效组织的去重候选。
// 参数：ctx/actor为同租户真实管理主体。
// 返回值：USER/ORG稳定目标列表；超过受控分页上限或依赖错误不返回部分成功。
func (d *localCatalogTargetDirectory) ListCatalogTargets(ctx context.Context, actor application.Principal) ([]contract.LocalCatalogTarget, error) {
	values := map[string]contract.LocalCatalogTarget{}
	for page := 1; page <= 100; page++ {
		users, total, err := d.query(ctx, actor, url.Values{"role_code": {"sales"}, "page": {fmt.Sprint(page)}, "page_size": {"50"}})
		if err != nil {
			return nil, err
		}
		for _, user := range users {
			values["USER:"+user.ID] = contract.LocalCatalogTarget{SubjectType: "USER", SubjectID: user.ID, Name: user.Name}
			for _, org := range user.Organizations {
				values["ORG:"+org.ID] = contract.LocalCatalogTarget{SubjectType: "ORG", SubjectID: org.ID, Name: org.Name}
			}
		}
		if int64(page*50) >= total {
			result := make([]contract.LocalCatalogTarget, 0, len(values))
			for _, value := range values {
				result = append(result, value)
			}
			sort.Slice(result, func(i, j int) bool {
				return result[i].SubjectType+result[i].Name+result[i].SubjectID < result[j].SubjectType+result[j].Name+result[j].SubjectID
			})
			return result, nil
		}
		if len(users) == 0 {
			return nil, application.ErrPersonnelDirectoryUnavailable
		}
	}
	return nil, application.ErrPersonnelDirectoryUnavailable
}

// CurrentCatalogOrganizations 精确读取本人当前有效组织，不借合同ORG授权推断会员关系。
// 参数：ctx/actor为当前真实会话主体。
// 返回值：当前会员组织ID，用户已失效或依赖异常失败关闭。
func (d *localCatalogTargetDirectory) CurrentCatalogOrganizations(ctx context.Context, actor application.Principal) ([]string, error) {
	if actor.IdentityID == "" {
		return nil, application.ErrForbidden
	}
	users, _, err := d.query(ctx, actor, url.Values{"user_id": {actor.IdentityID}, "page": {"1"}, "page_size": {"1"}})
	if err != nil {
		return nil, err
	}
	if len(users) != 1 || users[0].ID != actor.IdentityID {
		return nil, application.ErrPersonnelDirectoryUnavailable
	}
	result := []string{}
	for _, org := range users[0].Organizations {
		result = append(result, org.ID)
	}
	return result, nil
}
