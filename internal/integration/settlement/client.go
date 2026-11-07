package settlement

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	core "github.com/J-S-Te/license-core"

	"github.com/j-s-te/contract-management/internal/lifecycle"
)

// 合同 → 结算的财务生效事件投递。
//
// 结算侧固定入站端点为 POST /internal/v1/settlement/events/contracts，
// 事件类型 contract.financial_effective.v1，需要能解析出可信租户的机器 Bearer 令牌；
// 来源标识由结算侧硬编码为 contract_management，投递方不需要也不应该声明。
// 本投递器与 internal/integration/project 保持同一形态（认领/成功/失败/死信 + 许可门禁）。

type Delivery struct {
	ID       string
	TenantID string
	Payload  []byte
	Attempts uint
}

type Store interface {
	ClaimFinancialEffectiveDelivery(context.Context) (Delivery, bool, error)
	MarkFinancialEffectiveDelivered(context.Context, string) error
	MarkFinancialEffectiveFailed(context.Context, string, string, uint, bool) error
}

type responsibilityStore interface {
	ClaimContractResponsibilityDelivery(context.Context) (Delivery, bool, error)
	MarkContractResponsibilityDelivered(context.Context, string) error
	MarkContractResponsibilityFailed(context.Context, string, string, uint, bool) error
	ReconcileContractResponsibilities(context.Context) (int, error)
}

type deliveryReconciler interface {
	ReconcileFinancialEffectives(context.Context) (int, error)
}

// financialBacklogCounter 由仓储实现，统计“已生效但没有任何财务出站事件”的合同。
// 它把“审批时未维护付款计划所以不产生事件”的静默缺口暴露成可观测告警；
// 只是只读计数，不写入、不合成任何事件。
type financialBacklogCounter interface {
	FinancialEffectiveBacklog(context.Context) (int, error)
}

// batchFinancialStore 由仓储实现，按积压批量认领财务事件。
// 逐行仍各自写租约与 attempts，批量只放大单轮吞吐，不改变每事件幂等语义。
type batchFinancialStore interface {
	ClaimFinancialEffectiveDeliveries(context.Context, int) ([]Delivery, error)
}

// batchResponsibilityStore 由仓储实现，按积压批量认领责任事件。
type batchResponsibilityStore interface {
	ClaimContractResponsibilityDeliveries(context.Context, int) ([]Delivery, error)
}

// 批量与补建的默认参数。批量上限用于避免一次认领拖成长事务/长轮次，
// 补建周期用于消灭“只在进程启动补建一次”的缺口。
const (
	defaultDeliveryBatchSize  = 25
	maxDeliveryBatchSize      = 200
	defaultReconcileInterval  = 5 * time.Minute
	minDispatcherPollInterval = time.Second
)

type Dispatcher struct {
	LicenseGate interface {
		Check(context.Context, core.Operation) error
	}
	Store       Store
	BaseURL     string
	MaxAttempts uint
	Poll        time.Duration
	Client      *http.Client
	Logger      *slog.Logger
	// BatchSize 是每轮每类队列的认领上限（<=0 用默认 25，超过 200 按 200 封顶）。
	// 上限保证单轮工作量有界，不会退化成无界 goroutine 或长事务。
	BatchSize int
	// ReconcileEvery 是历史合同补建周期（<=0 用默认 5 分钟）。
	// 补建不再只在启动执行一次：付款计划补录、条件后来满足都能在周期内补建，
	// 无需重启进程。
	ReconcileEvery time.Duration
	// TokenSource 提供机器访问令牌。为空时不携带 Authorization——仅允许在未启用
	// 结算集成或本地测试时出现；结算侧会以 401 失败关闭，不会被静默接受。
	TokenSource func(context.Context) (string, error)
}

// Run 运行财务和责任事件的持久化投递循环。
// 参数：ctx 为进程生命周期。
// 返回值：无。
//
// 每轮按 BatchSize 有界批量认领，逐条投递并在单条失败时只回写该行，
// 因此批量导入/集中审批不会因为“每 2s 只投 1 条”而形成长期背压。
// 补建按 ReconcileEvery 周期执行：失败立即在下一轮重试，成功后按周期再跑，
// 不再“只在进程启动补建一次”。
func (d *Dispatcher) Run(ctx context.Context) {
	if d.Store == nil {
		return
	}
	poll := d.Poll
	if poll <= 0 {
		poll = minDispatcherPollInterval
	}
	reconcileEvery := d.ReconcileEvery
	if reconcileEvery <= 0 {
		reconcileEvery = defaultReconcileInterval
	}
	var reconcileAt time.Time // 零值表示“立即可补建”

	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		// 收到 SIGTERM 后不再开启新一轮认领；在途批次终态由 deliver 用独立 ctx 落库。
		if ctx.Err() != nil {
			return
		}
		if !time.Now().Before(reconcileAt) {
			reconcileAt = d.reconcileCycle(ctx, reconcileEvery)
		}

		if err := d.dispatch(ctx, d.batchSize()); err != nil && ctx.Err() == nil && d.Logger != nil {
			d.Logger.Error("dispatch settlement events", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// reconcileCycle 执行一次补建（含财务事件缺口告警），返回下一次补建时间。
// 参数：ctx 控制存储操作，every 为成功后的补建周期。
// 返回值：下一次补建时间；失败时返回当前时间，使下一轮立即重试。
func (d *Dispatcher) reconcileCycle(ctx context.Context, every time.Duration) time.Time {
	count, err := d.reconcile(ctx)
	if err != nil {
		if ctx.Err() == nil && d.Logger != nil {
			d.Logger.Error("reconcile settlement deliveries", "error", err)
		}
		return time.Now()
	}
	if count > 0 && d.Logger != nil {
		d.Logger.Info("historical settlement deliveries queued", "count", count)
	}
	if counter, ok := d.Store.(financialBacklogCounter); ok {
		backlog, backlogErr := counter.FinancialEffectiveBacklog(ctx)
		switch {
		case backlogErr != nil:
			// 计数失败只影响告警可见性，不能在补建成功后掩盖为“无积压”。
			if ctx.Err() == nil && d.Logger != nil {
				d.Logger.Warn("count effective contracts without financial event", "error", backlogErr)
			}
		case backlog > 0 && d.Logger != nil:
			d.Logger.Warn("effective contracts without financial effective event",
				"backlog", backlog,
				"hint", "维护合法付款计划（合计等于合同金额）后由周期补建生成事件，无需重启")
		}
	}
	return time.Now().Add(every)
}

// reconcile 补建现有合同缺失的责任及财务事件。
// 参数：ctx 控制存储操作。
// 返回值：新增数量及存储或许可错误。
func (d *Dispatcher) reconcile(ctx context.Context) (int, error) {
	if d.LicenseGate != nil {
		if err := d.LicenseGate.Check(ctx, core.MUTATE_BUSINESS); err != nil {
			return 0, err
		}
	}
	responsibilityCount := 0
	if store, ok := d.Store.(responsibilityStore); ok {
		var err error
		responsibilityCount, err = store.ReconcileContractResponsibilities(ctx)
		if err != nil {
			return 0, err
		}
	}
	reconciler, ok := d.Store.(deliveryReconciler)
	if !ok {
		return responsibilityCount, nil
	}
	count, err := reconciler.ReconcileFinancialEffectives(ctx)
	return responsibilityCount + count, err
}

// batchSize 返回本轮每类队列的认领上限；越界值收敛到有界区间。
// 参数：无。
// 返回值：1~maxDeliveryBatchSize 之间的批量上限。
func (d *Dispatcher) batchSize() int {
	size := d.BatchSize
	if size <= 0 {
		return defaultDeliveryBatchSize
	}
	if size > maxDeliveryBatchSize {
		return maxDeliveryBatchSize
	}
	return size
}

// dispatchOne 投递至多一条责任事件和一条财务事件。
// 参数：ctx 控制请求及持久化重试。
// 返回值：首个投递、许可或重试记录错误。
func (d *Dispatcher) dispatchOne(ctx context.Context) error {
	return d.dispatch(ctx, 1)
}

// dispatch 在有界批量内投递责任与财务事件。
// 参数：ctx 控制请求与持久化，limit 为每类队列本轮认领上限。
// 返回值：首个投递错误。单条失败只回写该行，不影响同批其它事件。
func (d *Dispatcher) dispatch(ctx context.Context, limit int) error {
	var firstErr error
	for _, step := range []func(context.Context, int) error{d.dispatchResponsibilities, d.dispatchFinancial} {
		if err := step(ctx, limit); err != nil && firstErr == nil {
			firstErr = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return firstErr
}

// dispatchResponsibilities 批量认领并投递责任事件。
// 参数：ctx 控制请求，limit 为本轮认领上限。
// 返回值：认领或投递的首个错误。
func (d *Dispatcher) dispatchResponsibilities(ctx context.Context, limit int) error {
	store, ok := d.Store.(responsibilityStore)
	if !ok {
		return nil
	}
	if d.LicenseGate != nil {
		if err := d.LicenseGate.Check(ctx, core.MUTATE_BUSINESS); err != nil {
			return err
		}
	}
	deliveries, err := claimResponsibilities(ctx, store, limit)
	if err != nil {
		return err
	}
	return d.deliverAll(ctx, deliveries, store.MarkContractResponsibilityDelivered, store.MarkContractResponsibilityFailed)
}

// dispatchFinancial 批量认领并投递财务生效事件。
// 参数：ctx 控制请求，limit 为本轮认领上限。
// 返回值：认领或投递的首个错误。
func (d *Dispatcher) dispatchFinancial(ctx context.Context, limit int) error {
	if d.LicenseGate != nil {
		if err := d.LicenseGate.Check(ctx, core.MUTATE_BUSINESS); err != nil {
			return err
		}
	}
	deliveries, err := claimFinancial(ctx, d.Store, limit)
	if err != nil {
		return err
	}
	return d.deliverAll(ctx, deliveries, d.Store.MarkFinancialEffectiveDelivered, d.Store.MarkFinancialEffectiveFailed)
}

// claimFinancial 优先使用仓储的批量认领能力；旧实现退化为单条认领。
// 参数：ctx 控制认领事务，store 为投递仓储，limit 为本轮上限。
// 返回值：认领行及存储错误。
func claimFinancial(ctx context.Context, store Store, limit int) ([]Delivery, error) {
	if batch, ok := store.(batchFinancialStore); ok {
		return batch.ClaimFinancialEffectiveDeliveries(ctx, limit)
	}
	delivery, found, err := store.ClaimFinancialEffectiveDelivery(ctx)
	if err != nil || !found {
		return nil, err
	}
	return []Delivery{delivery}, nil
}

// claimResponsibilities 优先使用仓储的批量认领能力；旧实现退化为单条认领。
// 参数：ctx 控制认领事务，store 为责任投递仓储，limit 为本轮上限。
// 返回值：认领行及存储错误。
func claimResponsibilities(ctx context.Context, store responsibilityStore, limit int) ([]Delivery, error) {
	if batch, ok := store.(batchResponsibilityStore); ok {
		return batch.ClaimContractResponsibilityDeliveries(ctx, limit)
	}
	delivery, found, err := store.ClaimContractResponsibilityDelivery(ctx)
	if err != nil || !found {
		return nil, err
	}
	return []Delivery{delivery}, nil
}

// deliverAll 逐条投递已认领事件并隔离单条失败。
// 参数：ctx 控制请求，deliveries 为本轮已认领行，delivered/failed 写回各自队列。
// 返回值：首个投递错误；其余行仍照常投递（失败行已单独持久化退避/死信）。
// 许可门禁在投递中被拒时不写失败回写（不消耗退避预算），该行及其余已认领行
// 由 5 分钟认领租约回收后重新投递，与单条投递语义一致，不会丢事件。
func (d *Dispatcher) deliverAll(ctx context.Context, deliveries []Delivery, delivered func(context.Context, string) error, failed func(context.Context, string, string, uint, bool) error) error {
	var firstErr error
	for _, delivery := range deliveries {
		if ctx.Err() != nil {
			// 进程退出时不再发起新请求；未投递行保留租约，由租约过期后重新认领，
			// 既不会丢事件，也不会在退出过程中堆积新请求。
			return firstErr
		}
		if err := d.deliver(ctx, delivery, delivered, failed); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if d.Logger != nil {
				d.Logger.Error("deliver settlement event",
					"delivery_id", delivery.ID, "attempts", delivery.Attempts, "error", err)
			}
		}
	}
	return firstErr
}

// deliver 按事件类型发送冻结载荷并持久记录成功或失败。
// 参数：ctx 控制请求，delivery 为队列行，delivered 和 failed 写回各自队列。
// 返回值：网络、载荷或持久化错误。
//
// 死信分类（BIZ-XEVT2-05 第 4 点）：结算侧返回 4xx 且不是 408/429 时是
// **确定性拒绝**（载荷/身份/状态语义问题），重试只会空转到 MaxAttempts；
// 因此一次即判永久死信，交给运维面处理。408/429、5xx、网络与令牌错误
// 仍按既有退避重试，避免把暂时性故障误判成永久失败。
func (d *Dispatcher) deliver(ctx context.Context, delivery Delivery, delivered func(context.Context, string) error, failed func(context.Context, string, string, uint, bool) error) error {
	var event struct {
		EventType string `json:"event_type"`
	}
	_ = json.Unmarshal(delivery.Payload, &event)
	endpoint := "/internal/v1/settlement/events/contracts"
	if event.EventType == "contract.responsibility_changed.v1" {
		endpoint = "/internal/v1/settlement/events/contract-responsibility"
	}
	permanent := false

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(d.BaseURL, "/")+endpoint,
		bytes.NewReader(delivery.Payload))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Financial-Effective-Delivery-ID", delivery.ID)
		req.Header.Set("X-Financial-Effective-Tenant-ID", delivery.TenantID)
		if d.TokenSource != nil {
			// 令牌获取失败必须走与 HTTP 失败相同的重试/死信持久化路径，避免投递被静默丢失。
			token, tokenErr := d.TokenSource(ctx)
			if tokenErr != nil {
				err = fmt.Errorf("fetch settlement integration token: %w", tokenErr)
			} else {
				req.Header.Set("Authorization", "Bearer "+token)
			}
		}
		if err == nil {
			client := d.Client
			if client == nil {
				client = &http.Client{Timeout: 15 * time.Second}
			}
			if d.LicenseGate != nil {
				if licenseErr := d.LicenseGate.Check(ctx, core.MUTATE_BUSINESS); licenseErr != nil {
					return licenseErr
				}
			}
			var response *http.Response
			response, err = client.Do(req)
			if err == nil {
				defer response.Body.Close()
				body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
				if response.StatusCode >= 200 && response.StatusCode < 300 {
					// 退出期间也必须写入终态：不能把已投递记录留在认领状态等租约。
					persistCtx, cancel := lifecycle.PersistContext(ctx)
					defer cancel()
					return delivered(persistCtx, delivery.ID)
				}
				permanent = permanentDeliveryFailure(response.StatusCode)
				err = fmt.Errorf("settlement API returned %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
			}
		}
	}
	dead := permanent || delivery.Attempts >= d.MaxAttempts
	persistCtx, cancel := lifecycle.PersistContext(ctx)
	defer cancel()
	if markErr := failed(persistCtx, delivery.ID, err.Error(), delivery.Attempts, dead); markErr != nil {
		return fmt.Errorf("delivery error: %v; persist retry: %w", err, markErr)
	}
	return err
}

// permanentDeliveryFailure 判断结算侧 HTTP 状态是否为确定性拒绝。
// 参数：status 为响应码。
// 返回值：4xx（408 请求超时、429 限流除外）返回 true，其余一律按可重试处理。
func permanentDeliveryFailure(status int) bool {
	if status < 400 || status >= 500 {
		return false
	}
	return status != http.StatusRequestTimeout && status != http.StatusTooManyRequests
}
