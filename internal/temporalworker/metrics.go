package temporalworker

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.temporal.io/sdk/client"
)

// MetricsRegistry 接收 Temporal SDK 指标并以 Prometheus 文本格式暴露。标签来自 SDK
// 的固定任务队列、工作流和活动类型，不接受业务输入，避免无限标签基数。
type MetricsRegistry struct {
	mu       sync.RWMutex
	counters map[string]float64
	gauges   map[string]float64
	timers   map[string]timerValue
	tags     map[string]string
}

type timerValue struct {
	Count float64
	Sum   float64
}

// NewMetricsRegistry 创建合同 Worker 私有的内存指标注册表。
func NewMetricsRegistry() *MetricsRegistry {
	return &MetricsRegistry{counters: map[string]float64{}, gauges: map[string]float64{}, timers: map[string]timerValue{}, tags: map[string]string{}}
}

// WithTags 返回携带合并标签的轻量视图，底层指标存储仍与父注册表共享。
func (registry *MetricsRegistry) WithTags(tags map[string]string) client.MetricsHandler {
	merged := cloneTags(registry.tags)
	for key, value := range tags {
		merged[key] = value
	}
	return &taggedMetrics{root: registry, tags: merged}
}

// Counter 获取一个累计计数器。
func (registry *MetricsRegistry) Counter(name string) client.MetricsCounter {
	return metricCounter{root: registry, key: metricKey(name, registry.tags)}
}

// Gauge 获取一个瞬时值指标。
func (registry *MetricsRegistry) Gauge(name string) client.MetricsGauge {
	return metricGauge{root: registry, key: metricKey(name, registry.tags)}
}

// Timer 获取一个以秒为单位累计次数和耗时的计时器。
func (registry *MetricsRegistry) Timer(name string) client.MetricsTimer {
	return metricTimer{root: registry, key: metricKey(name, registry.tags)}
}

type taggedMetrics struct {
	root *MetricsRegistry
	tags map[string]string
}

func (handler *taggedMetrics) WithTags(tags map[string]string) client.MetricsHandler {
	merged := cloneTags(handler.tags)
	for key, value := range tags {
		merged[key] = value
	}
	return &taggedMetrics{root: handler.root, tags: merged}
}
func (handler *taggedMetrics) Counter(name string) client.MetricsCounter {
	return metricCounter{root: handler.root, key: metricKey(name, handler.tags)}
}
func (handler *taggedMetrics) Gauge(name string) client.MetricsGauge {
	return metricGauge{root: handler.root, key: metricKey(name, handler.tags)}
}
func (handler *taggedMetrics) Timer(name string) client.MetricsTimer {
	return metricTimer{root: handler.root, key: metricKey(name, handler.tags)}
}

type metricCounter struct {
	root *MetricsRegistry
	key  string
}

func (counter metricCounter) Inc(delta int64) {
	counter.root.mu.Lock()
	counter.root.counters[counter.key] += float64(delta)
	counter.root.mu.Unlock()
}

type metricGauge struct {
	root *MetricsRegistry
	key  string
}

func (gauge metricGauge) Update(value float64) {
	gauge.root.mu.Lock()
	gauge.root.gauges[gauge.key] = value
	gauge.root.mu.Unlock()
}

type metricTimer struct {
	root *MetricsRegistry
	key  string
}

func (timer metricTimer) Record(duration time.Duration) {
	timer.root.mu.Lock()
	value := timer.root.timers[timer.key]
	value.Count++
	value.Sum += duration.Seconds()
	timer.root.timers[timer.key] = value
	timer.root.mu.Unlock()
}

// ServeHTTP 输出 Prometheus 文本格式；其中 temporal_workflow_failed_total 是工作流失败
// 告警的权威 Worker 指标，不再用日志行数量代替。
func (registry *MetricsRegistry) ServeHTTP(writer http.ResponseWriter, _ *http.Request) {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	keys := make([]string, 0, len(registry.counters)+len(registry.gauges)+len(registry.timers))
	for key := range registry.counters {
		keys = append(keys, "c\x00"+key)
	}
	for key := range registry.gauges {
		keys = append(keys, "g\x00"+key)
	}
	for key := range registry.timers {
		keys = append(keys, "t\x00"+key)
	}
	sort.Strings(keys)
	for _, encoded := range keys {
		kind, key := encoded[:1], encoded[2:]
		switch kind {
		case "c":
			fmt.Fprintf(writer, "%s %s\n", prometheusCounterKey(key), strconv.FormatFloat(registry.counters[key], 'f', -1, 64))
		case "g":
			fmt.Fprintf(writer, "%s %s\n", prometheusKey(key), strconv.FormatFloat(registry.gauges[key], 'f', -1, 64))
		case "t":
			value := registry.timers[key]
			name, labels := splitMetricKey(key)
			fmt.Fprintf(writer, "%s_sum%s %s\n%s_count%s %s\n", name, labels, strconv.FormatFloat(value.Sum, 'f', -1, 64), name, labels, strconv.FormatFloat(value.Count, 'f', -1, 64))
		}
	}
}

// StartMetricsServer 启动 Worker 私有指标端口，并在上下文取消时优雅关闭。
func StartMetricsServer(ctx context.Context, address string, registry *MetricsRegistry, logger *slog.Logger) error {
	if registry == nil || logger == nil || strings.TrimSpace(address) == "" {
		return fmt.Errorf("Temporal metrics server requires address, registry and logger")
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", registry)
	server := &http.Server{Addr: address, Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			logger.Error("Temporal metrics server shutdown failed", "error", err)
		}
	}()
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("Temporal metrics server failed", "error", err)
		}
	}()
	return nil
}

func metricKey(name string, tags map[string]string) string {
	name = sanitizeMetricName(name)
	if len(tags) == 0 {
		return name
	}
	keys := make([]string, 0, len(tags))
	for key := range tags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, sanitizeMetricName(key)+`="`+escapeLabel(tags[key])+`"`)
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}
func prometheusKey(key string) string { return key }
func splitMetricKey(key string) (string, string) {
	if index := strings.IndexByte(key, '{'); index >= 0 {
		return key[:index], key[index:]
	}
	return key, ""
}
func prometheusCounterKey(key string) string {
	name, labels := splitMetricKey(key)
	if !strings.HasSuffix(name, "_total") {
		name += "_total"
	}
	return name + labels
}
func sanitizeMetricName(value string) string {
	var builder strings.Builder
	for index, r := range value {
		valid := r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || index > 0 && r >= '0' && r <= '9'
		if valid {
			builder.WriteRune(r)
		} else {
			builder.WriteByte('_')
		}
	}
	if builder.Len() == 0 {
		return "temporal_metric"
	}
	return builder.String()
}
func escapeLabel(value string) string {
	return strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\"", "\\\"").Replace(value)
}
func cloneTags(tags map[string]string) map[string]string {
	result := make(map[string]string, len(tags))
	for key, value := range tags {
		result[key] = value
	}
	return result
}
