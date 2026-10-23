package httpapi

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// auditBuffer 隔离强制审计下的写响应与敏感下载，审计确认之前不能提交响应。
//
// 安全理由（SEC-D4b）：gin 的审计中间件在业务 handler 执行完毕后才能拿到
// Report 的结果，而此时业务响应往往已经写给客户端、无法撤回。只有先把响应
// 缓冲在内存里，才能在审计事件写入失败时丢弃业务响应并返回 503，保证
// “审计写不进去 ⇒ 客户端拿不到成功”，杜绝强制审计模式下的静默无审计写入。
// 仅在 PLATFORM_AUDIT_REQUIRED/prod 强制模式下启用，非强制模式行为不变。
// DOCX 最大10 MiB，PDF最大20 MiB；32 MiB响应上限覆盖既有文件，同时避免
// 将任意大小的分块/流式响应持续积累在内存。超限后即使Handler忽略Write错误也拒绝交付。
const maxAuditResponseBytes = 32 << 20

var errAuditResponseTooLarge = errors.New("audited response exceeds capture limit")

type auditBuffer struct {
	orig    gin.ResponseWriter
	headers http.Header
	body    bytes.Buffer
	status  int
	size    int
	err     error
	head    bool
}

func newAuditBuffer(orig gin.ResponseWriter) *auditBuffer {
	return &auditBuffer{orig: orig, headers: orig.Header().Clone(), status: http.StatusOK, size: -1}
}

// flushTo 在审计上报成功后把缓冲的响应提交到真实连接：
// 先搬响应头，再写状态码与响应体，语义与 gin 自带 responseWriter 一致。
func (b *auditBuffer) flushTo(dst gin.ResponseWriter) {
	b.WriteHeaderNow()
	if b.head && b.headers.Get("Content-Length") == "" && b.status >= 200 && b.status != http.StatusNoContent && b.status != http.StatusNotModified {
		b.headers.Set("Content-Length", strconv.Itoa(b.size))
	}
	// Header是隔离副本，既要提交新增头，也要提交Handler对上游头的删除。
	clear(dst.Header())
	for key, values := range b.headers {
		dst.Header().Del(key)
		for _, value := range values {
			dst.Header().Add(key, value)
		}
	}
	dst.WriteHeader(b.status)
	if !b.head && b.body.Len() > 0 {
		_, _ = dst.Write(b.body.Bytes())
		return
	}
	dst.WriteHeaderNow()
}

// —— 以下实现 gin.ResponseWriter 接口，行为对齐 gin 的 responseWriter。 ——

func (b *auditBuffer) Header() http.Header { return b.headers }

func (b *auditBuffer) WriteHeader(code int) {
	if code > 0 && b.status != code && !b.Written() {
		b.status = code
	}
}

func (b *auditBuffer) WriteHeaderNow() {
	if !b.Written() {
		b.size = 0
	}
}

func (b *auditBuffer) Write(p []byte) (int, error) {
	if err := b.checkSize(len(p)); err != nil {
		return 0, err
	}
	if b.head {
		b.size += len(p)
		return len(p), nil
	}
	n, err := b.body.Write(p)
	b.size += n
	return n, err
}

func (b *auditBuffer) WriteString(s string) (int, error) {
	if err := b.checkSize(len(s)); err != nil {
		return 0, err
	}
	if b.head {
		b.size += len(s)
		return len(s), nil
	}
	n, err := b.body.WriteString(s)
	b.size += n
	return n, err
}

func (b *auditBuffer) checkSize(n int) error {
	b.WriteHeaderNow()
	if b.err == nil && n > maxAuditResponseBytes-b.size {
		b.err = errAuditResponseTooLarge
	}
	return b.err
}

func (b *auditBuffer) Status() int   { return b.status }
func (b *auditBuffer) Size() int     { return b.size }
func (b *auditBuffer) Written() bool { return b.size != -1 }

// Flush 只标记“已写”，不能把半成品响应提前提交到真实连接，
// 否则审计失败时将无法撤回，拒绝语义会被破坏。
func (b *auditBuffer) Flush() { b.WriteHeaderNow() }

// Upgrade/server-push和底层writer会绕过隔离缓冲；这些响应不适用于下载/写接口。
func (b *auditBuffer) Pusher() http.Pusher { return nil }

func (b *auditBuffer) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, http.ErrNotSupported
}

func (b *auditBuffer) CloseNotify() <-chan bool { return b.orig.CloseNotify() }
