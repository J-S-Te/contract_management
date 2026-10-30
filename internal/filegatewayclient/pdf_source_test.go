package filegatewayclient

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPDFSourceUsesFormalPurpose 验证真实上传请求的用途、MIME和原件。
// 参数：t为HTTP客户端协议测试上下文。
// 返回值：无；PDF被当DOCX或未使用正式用途时测试失败。
func TestPDFSourceUsesFormalPurpose(t *testing.T) {
	document := []byte("%PDF-1.4\n%%EOF")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		defer r.MultipartForm.RemoveAll()
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		defer file.Close()
		body, _ := io.ReadAll(file)
		if r.FormValue("purpose") != "contract.external.pdf-source" ||
			r.FormValue("classification") != "CONFIDENTIAL" || header.Header.Get("Content-Type") != "application/pdf" ||
			!bytes.Equal(body, document) {
			t.Error("PDF上传协议与正式用途不一致")
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"file_id":"pdf-source-1"}}`))
	}))
	defer server.Close()
	client, err := New(server.URL, server.Client(), func(context.Context) (string, error) { return "test-machine", nil })
	if err != nil {
		t.Fatal(err)
	}
	if id, err := client.Upload(context.Background(), "source-stable", "contract", "CONTRACT_EXTERNAL_PDF_SOURCE",
		"原件.pdf", "application/pdf", bytes.NewReader(document)); err != nil || id != "pdf-source-1" {
		t.Fatalf("upload id=%q error=%v", id, err)
	}
}
