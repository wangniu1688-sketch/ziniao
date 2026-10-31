// Package apierr 统一错误码规范：跨模块一致的错误语义，避免各模块自造错误。
package apierr

import (
	"errors"
	"net/http"
)

// Code 机器可读错误码，前端与 SDK 依赖它做分支，不做字符串匹配。
type Code string

const (
	CodeUnauthorized   Code = "unauthorized"
	CodeForbidden      Code = "forbidden"
	CodeNotFound       Code = "not_found"
	CodeConflict       Code = "conflict"
	CodeValidation     Code = "validation_failed"
	CodeQuotaExceeded  Code = "quota_exceeded"
	CodeEntitlement    Code = "entitlement_required"
	CodeApprovalNeeded Code = "approval_required"
	CodeMFARequired    Code = "mfa_required"
	CodeRateLimited    Code = "rate_limited"
	CodeInternal       Code = "internal_error"
)

// Error 平台统一错误。
type Error struct {
	Code    Code   `json:"code"`
	HTTP    int    `json:"-"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

func New(code Code, httpStatus int, msg string) *Error {
	return &Error{Code: code, HTTP: httpStatus, Message: msg}
}

// 常用错误构造函数。权限相关一律 403 + 统一文案，避免信息泄露（哪一层被拒记在 Detail 供审计）。
func Unauthorized(msg string) *Error { return New(CodeUnauthorized, http.StatusUnauthorized, msg) }
func Forbidden(msg string) *Error    { return New(CodeForbidden, http.StatusForbidden, msg) }
func NotFound(msg string) *Error     { return New(CodeNotFound, http.StatusNotFound, msg) }
func Conflict(msg string) *Error     { return New(CodeConflict, http.StatusConflict, msg) }
func Validation(msg string) *Error   { return New(CodeValidation, http.StatusBadRequest, msg) }
func Internal(msg string) *Error     { return New(CodeInternal, http.StatusInternalServerError, msg) }
func Quota(msg string) *Error {
	return New(CodeQuotaExceeded, http.StatusPaymentRequired, msg)
}
func Entitlement(msg string) *Error {
	return New(CodeEntitlement, http.StatusPaymentRequired, msg)
}
func Approval(msg string) *Error {
	return New(CodeApprovalNeeded, http.StatusAccepted, msg)
}

// Is 允许 errors.Is 判断错误码。
func Is(err error, code Code) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Code == code
	}
	return false
}
