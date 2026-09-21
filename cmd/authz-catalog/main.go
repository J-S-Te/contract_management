// Command authz-catalog publishes the authorization catalog embedded in the
// contract-management image. Deployment tooling uses this one-shot command so
// the platform catalog is upgraded before a new API process starts accepting
// sign-ins.
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
