package pdf

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// AUD-2026-017：每次转换都会派生一个 libreoffice 重量级进程，无上限并发会耗尽
// 宿主机 CPU 与内存（影响同机其他服务）。包级信号量限制同时进行的转换数，
// 超出的请求在获取位置时阻塞等待；ctx 取消（客户端断开或超时）立即让出位置，
// 由调用方把 ErrConversionUnavailable 映射为 503。
const maxConcurrentConversions = 2

var conversionSlots = make(chan struct{}, maxConcurrentConversions)

// ErrConversionUnavailable 表示并发转换已达上限且未能在 ctx 截止前获得执行位置。
var ErrConversionUnavailable = errors.New("pdf conversion is busy, please retry later")

// convertCommand 以包级变量注入，测试用假命令替代 libreoffice，避免依赖真实安装。
var convertCommand = func(ctx context.Context, dir, input string) *exec.Cmd {
	return exec.CommandContext(ctx, "libreoffice", "--headless", "--convert-to", "pdf", "--outdir", dir, input)
}

func ConvertDOCX(ctx context.Context, document []byte) ([]byte, error) {
	select {
	case conversionSlots <- struct{}{}:
		defer func() { <-conversionSlots }()
	case <-ctx.Done():
		return nil, fmt.Errorf("acquire pdf conversion slot: %w: %v", ErrConversionUnavailable, ctx.Err())
	}
	dir, err := os.MkdirTemp("", "contract-pdf-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	input := filepath.Join(dir, "contract.docx")
	if err := os.WriteFile(input, document, 0o600); err != nil {
		return nil, err
	}
	cmd := convertCommand(ctx, dir, input)
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
