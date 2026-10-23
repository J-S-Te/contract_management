// Package auditctx 携带本次请求写入审计的来源信息（AUD-IP-2026）。
//
// HTTP 层在鉴权/审计中间件里写入，事务内的业务写操作读取它，使审计事件与业务变更
// 在同一事务落库时仍然带有真实的登录地址、操作地址与来源标记。
package auditctx

import "context"

// Meta 是请求级审计来源信息。
type Meta struct {
	LoginIP, ClientIP, IPSource, RequestID string
}

type metaKey struct{}

// WithMeta 写入来源信息。
func WithMeta(ctx context.Context, meta Meta) context.Context {
	return context.WithValue(ctx, metaKey{}, meta)
}

// FromContext 读取来源信息；不存在时返回零值。
func FromContext(ctx context.Context) Meta {
	meta, _ := ctx.Value(metaKey{}).(Meta)
	return meta
}
