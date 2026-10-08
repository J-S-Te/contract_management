package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadIncomeSources(t *testing.T) {
	items, err := loadSources("../../收入合同模版")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 5 {
		t.Fatalf("got %d templates, want 5", len(items))
	}
	for _, item := range items {
		if item.Fields < 10 || item.Name == "" {
			t.Fatalf("unexpected template %s", item.Name)
		}
	}
}

func TestLoadSourcesIgnoresWordLockFiles(t *testing.T) {
	items, err := loadSources("../../收入合同模版")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "template.docx"), items[0].Content, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "~$template.docx"), []byte("Word lock file"), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSources(dir)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("sources=%d error=%v", len(loaded), err)
	}
}

func TestLoadSourcesRejectsEmptyCorruptAndSymlink(t *testing.T) {
	for _, kind := range []string{"empty", "corrupt", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if kind == "corrupt" {
				if err := os.WriteFile(filepath.Join(dir, "bad.docx"), []byte("not OOXML"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "symlink" {
				if err := os.Symlink("/nonexistent", filepath.Join(dir, "bad.docx")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := loadSources(dir); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestUploadUsesAuthenticatedMultipartAndRejectsFailures(t *testing.T) {
	for _, status := range []int{201, 401, 403, 422, 500, 302} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/contract-templates" || r.Method != "POST" || r.Header.Get("Cookie") != "session=test" || r.Header.Get("Origin") != "http://localhost" {
					t.Error("wrong authenticated endpoint or origin")
				}
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Error(err)
				}
				if r.MultipartForm != nil {
					defer r.MultipartForm.RemoveAll()
				}
				if r.FormValue("name") != "模板" || len(r.MultipartForm.File["file"]) != 1 {
					t.Error("wrong multipart")
				}
				w.WriteHeader(status)
				fmt.Fprint(w, `{"code":"OK","data":{"id":"1","name":"模板"}}`)
			}))
			defer server.Close()
			i := importer{base: server.URL, origin: "http://localhost", cookie: "session=test", client: server.Client()}
			err := i.upload(context.Background(), source{Name: "模板", Filename: "template.docx", Content: []byte("data")})
			if (err == nil) != (status == 201) {
				t.Fatalf("status %d error %v", status, err)
			}
		})
	}
}

func TestUploadRejectsGatewayFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":"OK","data":{"id":"1","name":"模板","file_status":"FAILED"}}`)
	}))
	defer server.Close()
	i := importer{base: server.URL, client: server.Client()}
	if err := i.upload(context.Background(), source{Name: "模板", Filename: "template.docx"}); err == nil {
		t.Fatal("gateway failure must be visible")
	}
}
