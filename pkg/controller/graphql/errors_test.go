package graphql_test

import (
	"errors"
	"testing"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/gt"

	gqlctrl "github.com/m-mizutani/hecatoncheires/pkg/controller/graphql"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/semantic"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase"
)

func TestErrorCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil → empty", nil, ""},
		{"plain → empty", goerr.New("random"), ""},
		{"missing required on submit", goerr.Wrap(usecase.ErrMissingRequiredOnSubmit, "x"), gqlctrl.ErrCodeMissingRequiredFields},
		{"draft title required", goerr.Wrap(usecase.ErrDraftTitleRequired, "x"), gqlctrl.ErrCodeTitleRequired},
		{"case not draft (model)", goerr.Wrap(model.ErrCaseNotDraft, "x"), gqlctrl.ErrCodeInvalidStatusTransition},
		{"case not draft (usecase)", goerr.Wrap(usecase.ErrCaseNotDraft, "x"), gqlctrl.ErrCodeInvalidStatusTransition},
		{"case is draft", goerr.Wrap(usecase.ErrCaseIsDraft, "x"), gqlctrl.ErrCodeInvalidStatusTransition},
		{"thread-mode lifecycle via status", goerr.Wrap(usecase.ErrCaseThreadModeUseStatus, "x"), gqlctrl.ErrCodeInvalidStatusTransition},
		{"thread-mode case no actions", goerr.Wrap(usecase.ErrCaseThreadModeNoActions, "x"), gqlctrl.ErrCodeBadUserInput},
		{"private case in thread mode", goerr.Wrap(usecase.ErrCasePrivateThreadModeUnsupported, "x"), gqlctrl.ErrCodeBadUserInput},
		{"field validation failed", goerr.Wrap(usecase.ErrFieldValidationFailed, "x"), gqlctrl.ErrCodeFieldValidationFailed},
		{"activation failed", goerr.Wrap(usecase.ErrActivationFailed, "x"), gqlctrl.ErrCodeActivationFailed},
		// SubmitDraft joins ErrActivationFailed with the underlying sentinel; a
		// private-in-thread rejection must classify as the user-fixable
		// BAD_USER_INPUT, not the server-fault ACTIVATION_FAILED.
		{"private joined with activation failed → bad user input", errors.Join(usecase.ErrActivationFailed, goerr.Wrap(usecase.ErrCasePrivateThreadModeUnsupported, "x")), gqlctrl.ErrCodeBadUserInput},
		{"case not found", goerr.Wrap(usecase.ErrCaseNotFound, "x"), gqlctrl.ErrCodeNotFound},
		{"job not triggerable", goerr.Wrap(usecase.ErrJobNotFound, "x"), gqlctrl.ErrCodeNotFound},
		{"job already running", goerr.Wrap(usecase.ErrJobAlreadyRunning, "x"), gqlctrl.ErrCodeConflict},
		{"access denied", goerr.Wrap(usecase.ErrAccessDenied, "x"), gqlctrl.ErrCodeForbidden},
		{"workspace access denied", goerr.Wrap(model.ErrWorkspaceAccessDenied, "x"), gqlctrl.ErrCodeForbidden},
		{"already closed", goerr.Wrap(usecase.ErrCaseAlreadyClosed, "x"), gqlctrl.ErrCodeConflict},
		{"invalid argument", goerr.Wrap(usecase.ErrInvalidArgument, "x"), gqlctrl.ErrCodeBadUserInput},
		{"missing required (model)", goerr.Wrap(model.ErrMissingRequired, "x"), gqlctrl.ErrCodeBadUserInput},
		{"value breaks the field semantic", goerr.Wrap(semantic.ErrInvalidValue, "x"), gqlctrl.ErrCodeBadUserInput},
		{"value breaks the field pattern", goerr.Wrap(model.ErrTextPatternMismatch, "x"), gqlctrl.ErrCodeBadUserInput},
		{"workspace not found", goerr.Wrap(model.ErrWorkspaceNotFound, "x"), gqlctrl.ErrCodeNotFound},
		{"action comment not found", goerr.Wrap(usecase.ErrActionCommentNotFound, "x"), gqlctrl.ErrCodeNotFound},
		// The knowledge write path's rejections. Classifying them is what keeps
		// them visible: they carry errutil.TagBenign, so an unclassified one would
		// be answered 500 by statusForExtensionCode AND skipped by Sentry, leaving
		// an operator no record of a server fault the client was told about.
		{"invalid knowledge input", goerr.Wrap(usecase.ErrKnowledgeInput, "x"), gqlctrl.ErrCodeBadUserInput},
		{"unknown tag id", goerr.Wrap(usecase.ErrUnknownTag, "x"), gqlctrl.ErrCodeBadUserInput},
		{"tag still referenced", goerr.Wrap(usecase.ErrTagInUse, "x"), gqlctrl.ErrCodeConflict},
		{"slack channel not linkable", goerr.Wrap(usecase.ErrSlackChannelNotLinkable, "x"), gqlctrl.ErrCodeSlackChannelNotLinkable},
		{"slack channel already linked", goerr.Wrap(usecase.ErrSlackChannelAlreadyLinked, "x"), gqlctrl.ErrCodeSlackChannelLinked},
		{"slack channel join failed", goerr.Wrap(usecase.ErrSlackChannelJoinFailed, "x"), gqlctrl.ErrCodeSlackChannelJoinFailed},
		{"existing slack channel disabled", goerr.Wrap(usecase.ErrExistingSlackChannelDisabled, "x"), gqlctrl.ErrCodeBadUserInput},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gt.String(t, gqlctrl.ErrorCode(c.err)).Equal(c.want)
		})
	}
}

func TestErrorExtensions_MissingRequiredFields_IncludesNames(t *testing.T) {
	missingIDs := []string{"f1", "f2"}
	missingNames := []string{"Severity", "Reporter"}
	err := goerr.Wrap(usecase.ErrMissingRequiredOnSubmit, "missing",
		goerr.V(usecase.MissingFieldIDsKey, missingIDs),
		goerr.V(usecase.MissingFieldNamesKey, missingNames),
	)

	ext := gqlctrl.ErrorExtensions(err)
	gt.Value(t, ext[gqlctrl.ExtKeyCode]).Equal(gqlctrl.ErrCodeMissingRequiredFields)
	names, ok := ext[gqlctrl.ExtKeyMissingFieldNames].([]string)
	gt.Bool(t, ok).True().Required()
	gt.Array(t, names).Length(2)
	gt.String(t, names[0]).Equal("Severity")
	gt.String(t, names[1]).Equal("Reporter")
}

func TestErrorExtensions_MissingRequiredFields_OmitsNamesWhenEmpty(t *testing.T) {
	// When the field-names slice is empty we should not write the key at
	// all — extending it with [] would force the frontend to special-case
	// an empty array against missing.
	err := goerr.Wrap(usecase.ErrMissingRequiredOnSubmit, "missing")

	ext := gqlctrl.ErrorExtensions(err)
	gt.Value(t, ext[gqlctrl.ExtKeyCode]).Equal(gqlctrl.ErrCodeMissingRequiredFields)
	_, ok := ext[gqlctrl.ExtKeyMissingFieldNames]
	gt.Bool(t, ok).False()
}

func TestErrorExtensions_InvalidStatusTransition_IncludesCurrentStatus(t *testing.T) {
	err := goerr.Wrap(model.ErrCaseNotDraft, "cannot submit draft",
		goerr.V(usecase.CurrentStatusKey, "OPEN"),
	)

	ext := gqlctrl.ErrorExtensions(err)
	gt.Value(t, ext[gqlctrl.ExtKeyCode]).Equal(gqlctrl.ErrCodeInvalidStatusTransition)
	status, ok := ext[gqlctrl.ExtKeyCurrentStatus].(string)
	gt.Bool(t, ok).True().Required()
	gt.String(t, status).Equal("OPEN")
}

func TestErrorExtensions_InvalidStatusTransition_OmitsCurrentStatusWhenAbsent(t *testing.T) {
	err := goerr.Wrap(model.ErrCaseNotDraft, "cannot submit draft")

	ext := gqlctrl.ErrorExtensions(err)
	gt.Value(t, ext[gqlctrl.ExtKeyCode]).Equal(gqlctrl.ErrCodeInvalidStatusTransition)
	_, ok := ext[gqlctrl.ExtKeyCurrentStatus]
	gt.Bool(t, ok).False()
}

func TestErrorExtensions_Unclassified_ReturnsEmptyMap(t *testing.T) {
	ext := gqlctrl.ErrorExtensions(goerr.New("boom"))
	gt.Value(t, ext).NotNil()
	_, hasCode := ext[gqlctrl.ExtKeyCode]
	gt.Bool(t, hasCode).False()
}

func TestErrorExtensions_Nil_ReturnsEmptyMap(t *testing.T) {
	ext := gqlctrl.ErrorExtensions(nil)
	gt.Value(t, ext).NotNil()
	_, hasCode := ext[gqlctrl.ExtKeyCode]
	gt.Bool(t, hasCode).False()
}

func TestIsClientError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"missing required on submit → client", goerr.Wrap(usecase.ErrMissingRequiredOnSubmit, "x"), true},
		{"draft title required → client", goerr.Wrap(usecase.ErrDraftTitleRequired, "x"), true},
		{"case not draft → client", goerr.Wrap(model.ErrCaseNotDraft, "x"), true},
		{"field validation failed → client", goerr.Wrap(usecase.ErrFieldValidationFailed, "x"), true},
		{"access denied → client", goerr.Wrap(usecase.ErrAccessDenied, "x"), true},
		{"workspace access denied → client", goerr.Wrap(model.ErrWorkspaceAccessDenied, "x"), true},
		{"not found → client", goerr.Wrap(usecase.ErrCaseNotFound, "x"), true},
		{"activation failed → server", goerr.Wrap(usecase.ErrActivationFailed, "x"), false},
		{"unknown tag id → client", goerr.Wrap(usecase.ErrUnknownTag, "x"), true},
		{"tag still referenced → client", goerr.Wrap(usecase.ErrTagInUse, "x"), true},
		{"field pattern mismatch → client", goerr.Wrap(model.ErrTextPatternMismatch, "x"), true},
		{"random → server", goerr.New("boom"), false},
		{"slack channel not linkable → client", goerr.Wrap(usecase.ErrSlackChannelNotLinkable, "x"), true},
		{"slack channel already linked → client", goerr.Wrap(usecase.ErrSlackChannelAlreadyLinked, "x"), true},
		{"existing slack channel disabled → client", goerr.Wrap(usecase.ErrExistingSlackChannelDisabled, "x"), true},
		{"slack channel join failed → server", goerr.Wrap(usecase.ErrSlackChannelJoinFailed, "x"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.want {
				gt.Bool(t, gqlctrl.IsClientError(c.err)).True()
			} else {
				gt.Bool(t, gqlctrl.IsClientError(c.err)).False()
			}
		})
	}
}
