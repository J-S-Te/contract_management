package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/infrastructure/platform"
	"github.com/j-s-te/contract-management/internal/workflows"
)

type Identity interface {
	Authenticate(context.Context, *http.Request) (application.Principal, error)
}

type Handler struct {
	service  *application.Service
	identity Identity
}

func NewRouter(service *application.Service, identity Identity) http.Handler {
	h := &Handler{service: service, identity: identity}
	r := chi.NewRouter()
	r.Use(requestID, recoverer)
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, envelope{Code: "OK", Message: "ok", Data: map[string]string{"status": "up"}})
	})
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(h.authenticate)
		r.Post("/contracts", h.createContract)
		r.Get("/contracts/{contractID}", h.getContract)
		r.Post("/contracts/{contractID}/submit-approval", h.submitApproval)
		r.Post("/contracts/{contractID}/status-changes", h.changeStatus)
		r.Get("/approvals/tasks", h.listTasks)
		r.Get("/approvals/{approvalID}", h.getApproval)
		r.Get("/approval-rules", h.listRules)
		r.Post("/approval-rules", h.createRule)
		r.Put("/approval-rules/{ruleID}", h.updateRule)
		r.Delete("/approval-rules/{ruleID}", h.deleteRule)
		for _, action := range []string{"approve", "reject", "sign", "transfer", "return", "withdraw", "urge", "comments"} {
			r.Post("/approvals/{approvalID}/"+action, h.command(action))
		}
	})
	return r
}

func (h *Handler) listRules(w http.ResponseWriter, r *http.Request) {
	rules, err := h.service.ListRules(r.Context(), principal(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeData(w, r, http.StatusOK, rules)
}

func (h *Handler) createRule(w http.ResponseWriter, r *http.Request) {
	var rule approval.Rule
	if !decode(w, r, &rule) {
		return
	}
	created, err := h.service.CreateRule(r.Context(), principal(r), rule)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeData(w, r, http.StatusCreated, created)
}

func (h *Handler) updateRule(w http.ResponseWriter, r *http.Request) {
	var rule approval.Rule
	if !decode(w, r, &rule) {
		return
	}
	rule.ID = chi.URLParam(r, "ruleID")
	updated, err := h.service.UpdateRule(r.Context(), principal(r), rule)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeData(w, r, http.StatusOK, updated)
}

func (h *Handler) deleteRule(w http.ResponseWriter, r *http.Request) {
	version, err := strconv.ParseUint(r.URL.Query().Get("version"), 10, 64)
	if err != nil {
		writeEnvelopeError(w, r, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "version 参数不合法", nil)
		return
	}
	if err := h.service.DeleteRule(r.Context(), principal(r), chi.URLParam(r, "ruleID"), version); err != nil {
		writeError(w, r, err)
		return
	}
	writeData(w, r, http.StatusOK, map[string]string{"status": "deleted"})
}

type createContractRequest struct {
	Number              string     `json:"contract_number"`
	Title               string     `json:"title"`
	ContractType        string     `json:"contract_type"`
	ServiceType         string     `json:"service_type"`
	CustomerCreditLevel string     `json:"customer_credit_level"`
	AmountMinor         int64      `json:"amount_minor"`
	Currency            string     `json:"currency"`
	Content             string     `json:"content"`
	EndDate             *time.Time `json:"end_date"`
}

func (h *Handler) createContract(w http.ResponseWriter, r *http.Request) {
	var body createContractRequest
	if !decode(w, r, &body) {
		return
	}
	c, err := h.service.CreateContract(r.Context(), principal(r), contract.Contract{Number: body.Number, Title: body.Title, Type: body.ContractType, ServiceType: body.ServiceType, CustomerCreditLevel: body.CustomerCreditLevel, AmountMinor: body.AmountMinor, Currency: body.Currency, Content: body.Content, EndDate: body.EndDate})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeData(w, r, http.StatusCreated, c)
}

func (h *Handler) getContract(w http.ResponseWriter, r *http.Request) {
	c, err := h.service.GetContract(r.Context(), principal(r), chi.URLParam(r, "contractID"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeData(w, r, http.StatusOK, c)
}

func (h *Handler) submitApproval(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TermsIdentical bool `json:"terms_identical"`
	}
	if !decode(w, r, &body) {
		return
	}
	result, err := h.service.SubmitContract(r.Context(), principal(r), chi.URLParam(r, "contractID"), body.TermsIdentical)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeData(w, r, http.StatusAccepted, result)
}

func (h *Handler) changeStatus(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Version uint64          `json:"version"`
		Target  contract.Status `json:"target_status"`
		Reason  string          `json:"reason"`
	}
	if !decode(w, r, &body) {
		return
	}
	result, err := h.service.ChangeStatus(r.Context(), principal(r), chi.URLParam(r, "contractID"), body.Version, body.Target, strings.TrimSpace(body.Reason))
	if err != nil {
		writeError(w, r, err)
		return
	}
	if result.ApprovalID == "" {
		writeData(w, r, http.StatusOK, map[string]string{"status": "changed"})
		return
	}
	writeData(w, r, http.StatusAccepted, result)
}

func (h *Handler) listTasks(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	tasks, err := h.service.ListMyTasks(r.Context(), principal(r), limit)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeData(w, r, http.StatusOK, tasks)
}

func (h *Handler) getApproval(w http.ResponseWriter, r *http.Request) {
	state, err := h.service.GetApprovalState(r.Context(), principal(r), chi.URLParam(r, "approvalID"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeData(w, r, http.StatusOK, state)
}

type commandRequest struct {
	Comment       string                   `json:"comment"`
	TargetUserIDs []string                 `json:"target_user_ids"`
	Countersign   approval.CountersignMode `json:"countersign"`
	TargetNodeID  string                   `json:"target_node_id"`
}

func (h *Handler) command(pathAction string) http.HandlerFunc {
	actions := map[string]workflows.CommandAction{"approve": workflows.ActionApprove, "reject": workflows.ActionReject, "sign": workflows.ActionAddSign, "transfer": workflows.ActionTransfer, "return": workflows.ActionReturn, "withdraw": workflows.ActionWithdraw, "urge": workflows.ActionUrge, "comments": workflows.ActionComment}
	return func(w http.ResponseWriter, r *http.Request) {
		var body commandRequest
		if !decode(w, r, &body) {
			return
		}
		command := workflows.ApprovalCommand{Action: actions[pathAction], Comment: strings.TrimSpace(body.Comment), TargetUserIDs: body.TargetUserIDs, Countersign: body.Countersign, TargetNodeID: body.TargetNodeID}
		if err := h.service.Command(r.Context(), principal(r), chi.URLParam(r, "approvalID"), command); err != nil {
			writeError(w, r, err)
			return
		}
		writeData(w, r, http.StatusAccepted, map[string]string{"status": "accepted"})
	}
}

type contextKey string

const principalKey contextKey = "principal"

func (h *Handler) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := h.identity.Authenticate(r.Context(), r)
		if err != nil {
			writeError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}
func principal(r *http.Request) application.Principal {
	value, _ := r.Context().Value(principalKey).(application.Principal)
	return value
}

type envelope struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
	Data      any    `json:"data,omitempty"`
	Details   any    `json:"details,omitempty"`
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeEnvelopeError(w, r, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "请求参数不合法", err.Error())
		return false
	}
	if decoder.Decode(&struct{}{}) == nil {
		writeEnvelopeError(w, r, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "请求只能包含一个 JSON 对象", nil)
		return false
	}
	return true
}

func writeData(w http.ResponseWriter, r *http.Request, status int, data any) {
	writeJSON(w, status, envelope{Code: "OK", Message: "操作成功", RequestID: requestIDFrom(r.Context()), Data: data})
}
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, platform.ErrUnauthenticated):
		writeEnvelopeError(w, r, http.StatusUnauthorized, "AUTH_UNAUTHENTICATED", "登录状态无效", nil)
	case errors.Is(err, application.ErrForbidden):
		writeEnvelopeError(w, r, http.StatusForbidden, "AUTH_FORBIDDEN", "无权执行该操作", nil)
	case errors.Is(err, apperrors.ErrNotFound):
		writeEnvelopeError(w, r, http.StatusNotFound, "CON_NOT_FOUND", "资源不存在", nil)
	case errors.Is(err, apperrors.ErrVersionConflict):
		writeEnvelopeError(w, r, http.StatusConflict, "CON_VERSION_CONFLICT", "数据版本已变化，请刷新后重试", nil)
	case errors.Is(err, apperrors.ErrStateConflict), errors.Is(err, contract.ErrInvalidTransition):
		writeEnvelopeError(w, r, http.StatusConflict, "CON_STATE_CONFLICT", "当前状态不允许该操作", nil)
	case errors.Is(err, application.ErrValidation), errors.Is(err, contract.ErrInvalidStatus):
		writeEnvelopeError(w, r, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "请求参数不合法", nil)
	default:
		writeEnvelopeError(w, r, http.StatusInternalServerError, "CON_INTERNAL_ERROR", "服务暂时不可用", nil)
	}
}
func writeEnvelopeError(w http.ResponseWriter, r *http.Request, status int, code, message string, details any) {
	writeJSON(w, status, envelope{Code: code, Message: message, RequestID: requestIDFrom(r.Context()), Details: details})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type requestIDKey struct{}

func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if id == "" {
			id = fmt.Sprintf("con-%d", time.Now().UnixNano())
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}
func requestIDFrom(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey{}).(string)
	return value
}
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				writeEnvelopeError(w, r, http.StatusInternalServerError, "CON_INTERNAL_ERROR", "服务暂时不可用", nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
