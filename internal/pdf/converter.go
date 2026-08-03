package pdf

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func ConvertDOCX(ctx context.Context, document []byte) ([]byte, error) {
	dir, err := os.MkdirTemp("", "contract-pdf-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	input := filepath.Join(dir, "contract.docx")
	if err := os.WriteFile(input, document, 0o600); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "libreoffice", "--headless", "--convert-to", "pdf", "--outdir", dir, input)
	cmd.Env = append(os.Environ(), "HOME="+dir)
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("convert DOCX to PDF: %w: %s", err, output)
	}
	result, err := os.ReadFile(filepath.Join(dir, "contract.pdf"))
	if err != nil {
		return nil, fmt.Errorf("read converted PDF: %w", err)
	}
	if !Valid(result) {
		return nil, fmt.Errorf("converter returned an invalid PDF")
	}
	return result, nil
}

func Valid(document []byte) bool {
	if len(document) < 8 || len(document) > 20<<20 || string(document[:5]) != "%PDF-" {
		return false
	}
	tail := document
	if len(tail) > 2048 {
		tail = tail[len(tail)-2048:]
	}
	return bytes.Contains(tail, []byte("%%EOF"))
}
