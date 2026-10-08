// import-templates validates a DOCX directory and optionally imports it through
// the same authenticated, audited endpoint used by the contract template UI.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/docx"
)

type source struct {
	Name, Filename string
	Content        []byte
	Fields         int
}

func loadSources(directory string) ([]source, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	var result []source
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), "~$") || !strings.EqualFold(filepath.Ext(entry.Name()), ".docx") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > docx.MaxTemplateSize {
			return nil, fmt.Errorf("%s: must be a regular DOCX of at most 10MB", entry.Name())
		}
		file, err := os.Open(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		content, readErr := io.ReadAll(io.LimitReader(file, docx.MaxTemplateSize+1))
		closeErr := file.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if err := docx.ValidateExternalDocument(content); err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		fields, err := docx.Fields(content)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		result = append(result, source{Name: strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())), Filename: entry.Name(), Content: content, Fields: len(fields)})
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("no DOCX templates found")
	}
	return result, nil
}

type importer struct {
	client               *http.Client
	base, origin, cookie string
}

func (i importer) request(ctx context.Context, method, path, contentType string, body io.Reader, output any) error {
	req, err := http.NewRequestWithContext(ctx, method, i.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Cookie", i.cookie)
	req.Header.Set("Origin", i.origin)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	response, err := i.client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed (check service connectivity)")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d; inspect application audit/logs", method, path, response.StatusCode)
	}
	var envelope struct {
		Code string          `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&envelope); err != nil {
		return fmt.Errorf("invalid API envelope")
	}
	if envelope.Code != "OK" {
		return fmt.Errorf("API did not report success")
	}
	return json.Unmarshal(envelope.Data, output)
}

func (i importer) upload(ctx context.Context, item source) error {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("name", item.Name); err != nil {
		return err
	}
	part, err := writer.CreateFormFile("file", item.Filename)
	if err != nil {
		return err
	}
	if _, err := part.Write(item.Content); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	var created struct {
		ID, Name         string
		FileGatewayState string `json:"file_status"`
	}
	if err := i.request(ctx, http.MethodPost, "/api/v1/contract-templates", writer.FormDataContentType(), &body, &created); err != nil {
		return err
	}
	if created.ID == "" || created.Name != item.Name {
		return fmt.Errorf("upload response does not identify the expected template")
	}
	if created.FileGatewayState == "FAILED" {
		return fmt.Errorf("template %s was saved but file gateway failed; resolve before retrying", created.ID)
	}
	return nil
}

func run() error {
	dir := flag.String("dir", "收入合同模版", "directory containing DOCX templates")
	base := flag.String("url", "http://localhost:8081/contract_management", "contract application public URL")
	tenant := flag.String("tenant", "", "expected tenant ID (mandatory for apply)")
	cookieFile := flag.String("cookie-file", "", "0600 file containing the existing contract session Cookie header value")
	apply := flag.Bool("apply", false, "write templates; default only validates files locally")
	flag.Parse()
	items, err := loadSources(*dir)
	if err != nil {
		return err
	}
	for _, item := range items {
		fmt.Printf("VALID %s (%d fields)\n", item.Filename, item.Fields)
	}
	if !*apply {
		fmt.Println("Validation only: no network requests or database writes.")
		return nil
	}
	u, err := url.Parse(*base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("invalid public URL")
	}
	if *tenant == "" || *cookieFile == "" {
		return fmt.Errorf("apply requires expected -tenant and -cookie-file")
	}
	info, err := os.Stat(*cookieFile)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 16384 {
		return fmt.Errorf("cookie file must be a private regular file (0600) of at most 16KB")
	}
	secret, err := os.ReadFile(*cookieFile)
	if err != nil {
		return err
	}
	cookie := strings.TrimSpace(string(secret))
	if cookie == "" || strings.ContainsAny(cookie, "\r\n") {
		return fmt.Errorf("invalid cookie header")
	}
	i := importer{base: strings.TrimRight(u.String(), "/"), origin: u.Scheme + "://" + u.Host, cookie: cookie, client: &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	ctx := context.Background()
	var me struct {
		TenantID           string `json:"tenant_id"`
		Roles, Permissions []string
	}
	if err := i.request(ctx, http.MethodGet, "/api/v1/auth/me", "", nil, &me); err != nil {
		return err
	}
	if me.TenantID != *tenant {
		return fmt.Errorf("current session does not belong to expected tenant; nothing imported")
	}
	allowed := false
	for _, role := range me.Roles {
		allowed = allowed || role == "admin"
	}
	for _, permission := range me.Permissions {
		allowed = allowed || permission == "contract.template.manage"
	}
	if !allowed {
		return fmt.Errorf("current session cannot manage templates; nothing imported")
	}
	var existing []struct{ Name string }
	if err := i.request(ctx, http.MethodGet, "/api/v1/contract-templates", "", nil, &existing); err != nil {
		return err
	}
	names := make(map[string]bool)
	for _, item := range existing {
		names[item.Name] = true
	}
	for _, item := range items {
		if names[item.Name] {
			fmt.Printf("SKIP existing name: %s (not overwritten; contents not compared)\n", item.Name)
			continue
		}
		if err := i.upload(ctx, item); err != nil {
			return fmt.Errorf("import stopped at %s: %w; preceding successful imports are retained", item.Filename, err)
		}
		fmt.Printf("IMPORTED %s\n", item.Name)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
