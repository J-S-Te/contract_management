package pdf

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeConvertCommand(t *testing.T) {
	t.Helper()
	old := convertCommand
	convertCommand = func(ctx context.Context, dir, input string) *exec.Cmd {
		// 假命令不依赖 libreoffice：直接生成一份最小合法 PDF，
		// 引号定界符 heredoc 避免 shell 展开或格式化歧义。
		script := "cat > " + quoteShell(filepath.Join(dir, "contract.pdf")) + " <<'PDF'\n%PDF-1.7\n%%EOF\nPDF\n"
		return exec.CommandContext(ctx, "sh", "-c", script)
	}
	t.Cleanup(func() { convertCommand = old })
}

func quoteShell(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func TestConvertDOCXWaitsWhenConversionSlotsAreExhausted(t *testing.T) {
	fakeConvertCommand(t)
	for i := 0; i < maxConcurrentConversions; i++ {
		conversionSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for i := 0; i < maxConcurrentConversions; i++ {
			<-conversionSlots
		}
	})
	waitCtx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := ConvertDOCX(waitCtx, []byte("doc")); !errors.Is(err, ErrConversionUnavailable) {
		t.Fatalf("conversion while slots exhausted: err = %v, want ErrConversionUnavailable", err)
	}
}

func TestConvertDOCXRecoversAfterSlotsAreReleased(t *testing.T) {
	fakeConvertCommand(t)
	for i := 0; i < maxConcurrentConversions; i++ {
		conversionSlots <- struct{}{}
	}
	for i := 0; i < maxConcurrentConversions; i++ {
		<-conversionSlots
	}
	document, err := ConvertDOCX(context.Background(), []byte("doc"))
	if err != nil {
		t.Fatalf("conversion after slots released: %v", err)
	}
	if !Valid(document) {
		t.Fatal("converted document is not a valid PDF")
	}
	// 完成的转换必须归还槽位，否则后续请求会永久排队。
	for i := 0; i < maxConcurrentConversions; i++ {
		select {
		case conversionSlots <- struct{}{}:
			defer func() { <-conversionSlots }()
		default:
			t.Fatal("completed conversion did not release its slot")
		}
	}
}
