package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/filegatewayclient"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

type contractRow struct {
	ID, TenantID, Title, SourceFileStatus string
	RenderedDocument                      []byte
}

func (contractRow) TableName() string { return "con_contract" }

type templateRow struct {
	ID, TenantID, OriginalFilename, FileGatewayState string
	Document                                         []byte
}

func (templateRow) TableName() string { return "con_contract_template" }

func main() {
	limit := flag.Int("limit", 100, "maximum rows per table")
	interval := flag.Duration("interval", 250*time.Millisecond, "delay between gateway writes")
	flag.Parse()
	if *limit < 1 || *limit > 1000 || *interval < 0 {
		log.Fatal("invalid limit or interval")
	}
	dsn, baseURL, tokenURL := os.Getenv("MYSQL_DSN"), os.Getenv("FILE_GATEWAY_BASE_URL"), os.Getenv("FILE_GATEWAY_TOKEN_URL")
	clientID, secret, applicationID := os.Getenv("FILE_GATEWAY_CLIENT_ID"), os.Getenv("FILE_GATEWAY_CLIENT_SECRET"), os.Getenv("FILE_GATEWAY_APPLICATION_ID")
	if tokenURL == "" {
		tokenURL = strings.TrimRight(os.Getenv("PLATFORM_BASE_URL"), "/") + "/oauth2/token"
	}
	if dsn == "" || baseURL == "" || tokenURL == "" || clientID == "" || secret == "" || applicationID == "" {
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
	if err = db.WithContext(ctx).Where("template_id IS NULL AND rendered_document IS NOT NULL AND OCTET_LENGTH(rendered_document) > 0 AND (source_file_status = '' OR source_file_status <> 'READY')").Order("id").Limit(*limit).Find(&contracts).Error; err != nil {
		log.Fatal(err)
	}
	for _, row := range contracts {
		fileID, uploadErr := gateway.Upload(ctx, "backfill-contract-"+row.ID, applicationID, "CONTRACT_EXTERNAL_SOURCE", safeName(row.Title, row.ID)+".docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", bytes.NewReader(row.RenderedDocument))
		if uploadErr == nil {
			uploadErr = gateway.Bind(ctx, applicationID, fileID, "contract", row.ID, "EXTERNAL_SOURCE", safeName(row.Title, row.ID)+".docx")
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
	if err = db.WithContext(ctx).Where("document IS NOT NULL AND OCTET_LENGTH(document) > 0 AND (file_gateway_state = '' OR file_gateway_state <> 'READY')").Order("id").Limit(*limit).Find(&templates).Error; err != nil {
		log.Fatal(err)
	}
	for _, row := range templates {
		fileID, uploadErr := gateway.Upload(ctx, "backfill-template-"+row.ID, applicationID, "CONTRACT_TEMPLATE", row.OriginalFilename, "application/vnd.openxmlformats-officedocument.wordprocessingml.document", bytes.NewReader(row.Document))
		if uploadErr == nil {
			uploadErr = gateway.Bind(ctx, applicationID, fileID, "contract_template", row.ID, "TEMPLATE_SOURCE", row.OriginalFilename)
		}
		state, detail := "READY", ""
		if uploadErr != nil {
			state, detail, failed = "FAILED", uploadErr.Error(), failed+1
		}
		if len(detail) > 512 {
			detail = detail[:512]
		}
		if updateErr := db.WithContext(ctx).Model(&templateRow{}).Where("tenant_id = ? AND id = ?", row.TenantID, row.ID).Updates(map[string]any{"platform_file_id": fileID, "file_gateway_state": state, "file_gateway_last_error": detail}).Error; updateErr != nil {
			log.Fatal(updateErr)
		}
		processed++
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
