package platform

import "testing"

// TestLocalCatalogPermissionIsNotGeneralized 验证新增管理权限仅进入明确的admin目录角色。
// 参数：t为测试上下文，加载真实内嵌授权manifest。
// 返回值：无；既有其他角色不能隐式获得新目录写权限。
func TestLocalCatalogPermissionIsNotGeneralized(t *testing.T) {
	catalog, err := LoadAuthorizationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := catalog.Permissions["contract.catalog.manage"]; !ok {
		t.Fatal("local catalog permission missing")
	}
	for role, permissions := range catalog.RolePermissions {
		_, granted := permissions["contract.catalog.manage"]
		if granted != (role == "admin") {
			t.Fatalf("unexpected catalog privilege: role=%s granted=%v", role, granted)
		}
	}
}
