package graphql

import (
	"errors"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/semantic"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase"
)

// GraphQL extensions code values. The frontend matches on these literals to
// render error-specific UI (e.g. "you need to fill these required fields"
// vs. "the draft has already been opened"). Add new entries here — do not
// embed code literals at call sites.
const (
	ErrCodeBadUserInput            = "BAD_USER_INPUT"
	ErrCodeNotFound                = "NOT_FOUND"
	ErrCodeForbidden               = "FORBIDDEN"
	ErrCodeConflict                = "CONFLICT"
	ErrCodeUnauthenticated         = "UNAUTHENTICATED"
	ErrCodeMissingRequiredFields   = "MISSING_REQUIRED_FIELDS"
	ErrCodeTitleRequired           = "TITLE_REQUIRED"
	ErrCodeInvalidStatusTransition = "INVALID_STATUS_TRANSITION"
	ErrCodeFieldValidationFailed   = "FIELD_VALIDATION_FAILED"
	ErrCodeActivationFailed        = "ACTIVATION_FAILED"
	ErrCodeSlackChannelNotLinkable = "SLACK_CHANNEL_NOT_LINKABLE"
	ErrCodeSlackChannelLinked      = "SLACK_CHANNEL_ALREADY_LINKED"
	ErrCodeSlackChannelJoinFailed  = "SLACK_CHANNEL_JOIN_FAILED"
)

// GraphQL extensions field names. Each granular code is allowed to add
// code-specific keys in addition to "code"; the frontend reads them by name
// off `error.extensions`.
const (
	ExtKeyCode              = "code"
	ExtKeyMissingFieldNames = "missingFieldNames"
	ExtKeyCurrentStatus     = "currentStatus"
)

// ErrorCode returns the granular GraphQL code for err, or "" when the error
// is unclassified (which the HTTP layer treats as a server fault → 500).
//
// The ordering matters when an error chain matches multiple sentinels — the
// more specific code wins (e.g. ErrMissingRequiredOnSubmit before any
// generic CONFLICT classification).
func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	switch {
	// Draft-specific granular codes (highest priority — these errors
	// surface inside SubmitDraft / DiscardDraft).
	case errors.Is(err, usecase.ErrMissingRequiredOnSubmit):
		return ErrCodeMissingRequiredFields
	case errors.Is(err, usecase.ErrDraftTitleRequired):
		return ErrCodeTitleRequired
	case errors.Is(err, model.ErrCaseNotDraft),
		errors.Is(err, usecase.ErrCaseNotDraft),
		errors.Is(err, usecase.ErrCaseIsDraft),
		errors.Is(err, usecase.ErrCaseThreadModeUseStatus):
		return ErrCodeInvalidStatusTransition
	case errors.Is(err, usecase.ErrFieldValidationFailed):
		return ErrCodeFieldValidationFailed
	// A thread-mode private rejection is joined with ErrActivationFailed on the
	// SubmitDraft path (errors.Join). It is a user-fixable input error, so it must
	// classify as BAD_USER_INPUT ahead of the generic ACTIVATION_FAILED below.
	case errors.Is(err, usecase.ErrCasePrivateThreadModeUnsupported):
		return ErrCodeBadUserInput
	// Existing-channel rejections tell the create form which message to show
	// under the channel picker.
	case errors.Is(err, usecase.ErrSlackChannelNotLinkable):
		return ErrCodeSlackChannelNotLinkable
	case errors.Is(err, usecase.ErrSlackChannelAlreadyLinked):
		return ErrCodeSlackChannelLinked
	case errors.Is(err, usecase.ErrSlackChannelJoinFailed):
		return ErrCodeSlackChannelJoinFailed
	case errors.Is(err, usecase.ErrExistingSlackChannelDisabled):
		return ErrCodeBadUserInput
	case errors.Is(err, usecase.ErrActivationFailed):
		return ErrCodeActivationFailed

	// Generic categories — match existing HTTP status mapping.
	case errors.Is(err, model.ErrInvalidFieldType),
		errors.Is(err, model.ErrInvalidOptionID),
		errors.Is(err, semantic.ErrInvalidValue),
		errors.Is(err, model.ErrTextPatternMismatch),
		errors.Is(err, model.ErrMissingRequired),
		errors.Is(err, model.ErrCaseFieldValidation),
		errors.Is(err, model.ErrInvalidNotionID),
		errors.Is(err, model.ErrInvalidGitHubRepo),
		errors.Is(err, usecase.ErrUnknownUser),
		errors.Is(err, usecase.ErrInvalidArgument),
		errors.Is(err, usecase.ErrKnowledgeInput),
		errors.Is(err, usecase.ErrUnknownTag),
		errors.Is(err, usecase.ErrCaseThreadModeNoActions):
		return ErrCodeBadUserInput
	case errors.Is(err, usecase.ErrCaseNotFound),
		errors.Is(err, usecase.ErrActionNotFound),
		errors.Is(err, usecase.ErrActionStepNotFound),
		errors.Is(err, usecase.ErrActionCommentNotFound),
		errors.Is(err, usecase.ErrJobNotFound),
		errors.Is(err, model.ErrWorkspaceNotFound):
		return ErrCodeNotFound
	case errors.Is(err, usecase.ErrAccessDenied),
		errors.Is(err, model.ErrWorkspaceAccessDenied):
		return ErrCodeForbidden
	case errors.Is(err, usecase.ErrCaseAlreadyClosed),
		errors.Is(err, usecase.ErrCaseAlreadyOpen),
		errors.Is(err, usecase.ErrJobAlreadyRunning),
		errors.Is(err, usecase.ErrTagInUse),
		errors.Is(err, usecase.ErrDuplicateField):
		return ErrCodeConflict
	}
	return ""
}

// ErrorExtensions builds the GraphQL `extensions` map for err, including the
// code (when classifiable) and any code-specific detail fields the frontend
// needs to render a useful message. The caller (typically the gqlgen
// ErrorPresenter) merges the returned map into gqlerror.Extensions.
//
// Returns an empty (non-nil) map for unclassified errors so the caller can
// always merge unconditionally.
func ErrorExtensions(err error) map[string]any {
	out := map[string]any{}
	if err == nil {
		return out
	}
	code := ErrorCode(err)
	if code != "" {
		out[ExtKeyCode] = code
	}

	// Code-specific enrichment. Each branch pulls goerr.V-stored detail off
	// the error chain and exposes it under the documented extension key.
	values := errorValues(err)
	switch code {
	case ErrCodeMissingRequiredFields:
		if names, ok := values[usecase.MissingFieldNamesKey].([]string); ok && len(names) > 0 {
			out[ExtKeyMissingFieldNames] = names
		}
	case ErrCodeInvalidStatusTransition:
		if status, ok := values[usecase.CurrentStatusKey].(string); ok && status != "" {
			out[ExtKeyCurrentStatus] = status
		}
	}
	return out
}

// errorValues returns the merged goerr.V values along err's chain. Returns
// an empty map (never nil) when the chain has no goerr.Error.
func errorValues(err error) map[string]any {
	var ge *goerr.Error
	if !errors.As(err, &ge) {
		return map[string]any{}
	}
	return ge.Values()
}

// IsClientError reports whether err is a user-fault classification (4xx).
// Server-fault classifications (ACTIVATION_FAILED, SLACK_CHANNEL_JOIN_FAILED,
// unclassified) return false so the ErrorPresenter logs them at full
// severity; a failed join can mean the deployment lacks channels:join.
func IsClientError(err error) bool {
	switch ErrorCode(err) {
	case ErrCodeBadUserInput,
		ErrCodeNotFound,
		ErrCodeForbidden,
		ErrCodeConflict,
		ErrCodeUnauthenticated,
		ErrCodeMissingRequiredFields,
		ErrCodeTitleRequired,
		ErrCodeInvalidStatusTransition,
		ErrCodeFieldValidationFailed,
		ErrCodeSlackChannelNotLinkable,
		ErrCodeSlackChannelLinked:
		return true
	}
	return false
}
