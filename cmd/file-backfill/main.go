package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	core "github.com/J-S-Te/license-core"
	"github.com/J-S-Te/license-core/consumer"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/filegatewayclient"
	store "github.com/j-s-te/contract-management/internal/infrastructure/mysql"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

type contractRow struct {
	ID, TenantID, Title, SourceFileStatus        string
	DocumentFormat, SourceFilename, SourceSHA256 string
	RenderedDocument                             []byte
}

func (contractRow) TableName() string { return "con_contract" }

type templateRow struct {
	ID, TenantID, OriginalFilename, FileGatewayState string
	Document                                         []byte
}

func (templateRow) TableName() string { return "con_contract_template" }

// main 使用显式租户与许可上下文补偿源文件及模板的网关副本。
// 参数：无；凭据只从运行环境读取，limit与interval限制本次操作。
// 返回值：无；配置、许可或持久化失败以非零状态退出。
func main() {
	licenseGate, licenseErr := consumer.FromEnvironment("contract_management")
	if licenseErr != nil {
		log.Fatal("commercial license configuration failed")
	}
	limit := flag.Int("limit", 100, "maximum rows per table")
	interval := flag.Duration("interval", 250*time.Millisecond, "delay between gateway writes")
	flag.Parse()
	if *limit < 1 || *limit > 1000 || *interval < 0 {
		log.Fatal("invalid limit or interval")
	}
	dsn, baseURL, tokenURL := os.Getenv("MYSQL_DSN"), os.Getenv("FILE_GATEWAY_BASE_URL"), os.Getenv("FILE_GATEWAY_TOKEN_URL")
	clientID, secret, applicationID := os.Getenv("FILE_GATEWAY_CLIENT_ID"), os.Getenv("FILE_GATEWAY_CLIENT_SECRET"), os.Getenv("FILE_GATEWAY_APPLICATION_ID")
	tenantID := strings.TrimSpace(os.Getenv("OIDC_TENANT_ID"))
	if tokenURL == "" {
		tokenURL = strings.TrimRight(os.Getenv("PLATFORM_BASE_URL"), "/") + "/oauth2/token"
	}
	if dsn == "" || baseURL == "" || tokenURL == "" || clientID == "" || secret == "" || applicationID == "" || tenantID == "" {
		log.Fatal("database and file gateway configuration are required")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal(err)
	}
	httpClient := &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	oauth := clientcredentials.Config{ClientID: clientID, ClientSecret: secret, TokenURL: tokenURL, Scopes: []string{"platform:file:upload", "platform:file:bind"}}
	gateway, err := filegatewayclient.New(baseURL, httpClient, func(ctx context.Context) (string, error) {
		token, tokenErr := oauth.Token(context.WithValue(ctx, oauth2.HTTPClient, httpClient))
		if tokenErr != nil {
			return "", tokenErr
		}
		return token.AccessToken, nil
	})
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	processed, failed := 0, 0
	var contracts []contractRow
	if err = db.WithContext(ctx).Where("tenant_id = ?", tenantID).Where("template_id IS NULL AND rendered_document IS NOT NULL AND OCTET_LENGTH(rendered_document) > 0 AND (source_file_status = '' OR source_file_status <> 'READY')").Order("id").Limit(*limit).Find(&contracts).Error; err != nil {
		log.Fatal(err)
	}
	for _, row := range contracts {
		if licenseGate.Check(ctx, core.MUTATE_BUSINESS) != nil {
			log.Fatal("commercial license blocks file backfill")
		}
		purpose, filename, mediaType, uploadErr := sourceBackfillDescriptor(row)
		fileID := ""
		if uploadErr == nil {
			fileID, uploadErr = gateway.Upload(ctx, "backfill-contract-"+row.ID, applicationID, purpose, filename, mediaType, bytes.NewReader(row.RenderedDocument))
		}
		if uploadErr == nil {
			uploadErr = gateway.Bind(ctx, applicationID, fileID, "contract", row.ID, "EXTERNAL_SOURCE", filename)
		}
		state, detail := "READY", ""
		if uploadErr != nil {
			state, detail, failed = "FAILED", uploadErr.Error(), failed+1
		}
		if len(detail) > 512 {
			detail = detail[:512]
		}
		if updateErr := db.WithContext(ctx).Model(&contractRow{}).Where("tenant_id = ? AND id = ?", row.TenantID, row.ID).Updates(map[string]any{"source_file_id": fileID, "source_file_status": state, "source_file_last_error": detail}).Error; updateErr != nil {
			log.Fatal(updateErr)
		}
		processed++
		if *interval > 0 {
			time.Sleep(*interval)
		}
	}
	var templates []templateRow
	if err = db.WithContext(ctx).Where("tenant_id = ? AND document IS NOT NULL AND OCTET_LENGTH(document) > 0 AND gateway_source_id = ''", tenantID).Order("id").Limit(*limit).Find(&templates).Error; err != nil {
		log.Fatal(err)
	}
	repository := store.NewRepository(db)
	for _, row := range templates {
		if licenseGate.Check(ctx, core.MUTATE_BUSINESS) != nil {
			log.Fatal("commercial license blocks file backfill")
		}
		if queueErr := repository.QueueTemplateFileBackfill(ctx, row.TenantID, row.ID); queueErr != nil {
			log.Fatal(queueErr)
		}
		processed++
		if *interval > 0 {
			time.Sleep(*interval)
		}
	}
	// 仅投递已持久化意图，使用与 API 和工作器相同的恢复路径。
	dispatcher := application.TemplateFileDispatcher{Store: repository, Gateway: gateway, ApplicationID: applicationID, TenantID: tenantID, LicenseGate: licenseGate}
	for count := 0; count < *limit; count++ {
		found, deliveryErr := dispatcher.DeliverOnce(ctx)
		if deliveryErr != nil {
			failed++
			break
		}
		if !found {
			break
		}
		if *interval > 0 {
			time.Sleep(*interval)
		}
	}
	fmt.Printf("contract file backfill completed: processed=%d failed=%d\n", processed, failed)
	if failed > 0 {
		os.Exit(2)
	}
}

func safeName(value, fallback string) string {
	value = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(value, "/", "-"), "\\", "-"))
	if value == "" {
		return fallback
	}
	return value
}
