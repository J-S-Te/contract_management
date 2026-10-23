// Command authz-catalog 发布合同管理镜像中嵌入的
// 授权目录。部署工具使用该一次性命令，确保
// 新 API 进程开始接受登录前，平台目录
// 已完成升级。
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/j-s-te/contract-management/internal/infrastructure/platform"
)

func main() {
	if !isPublishCommand(os.Args[1:]) {
		fmt.Fprintln(os.Stderr, "usage: authz-catalog publish")
		os.Exit(2)
	}

	err := platform.SyncAuthorizationCatalog(context.Background(), platform.CatalogSyncOptions{
		Enabled:       true,
		BaseURL:       os.Getenv("PLATFORM_BASE_URL"),
		ApplicationID: os.Getenv("PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID"),
		ClientID:      os.Getenv("PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID"),
		ClientSecret:  os.Getenv("PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET"),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("authorization catalog published: application=contract_management")
}

func isPublishCommand(arguments []string) bool {
	return len(arguments) == 1 && strings.EqualFold(strings.TrimSpace(arguments[0]), "publish")
}
