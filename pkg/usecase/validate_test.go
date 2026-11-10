package usecase_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase"
)

func buildValidateTestSchema() *config.FieldSchema {
	return &config.FieldSchema{
		Fields: []config.FieldDefinition{
			{
				ID:   "title-text",
				Name: "Title Text",
				Type: types.FieldTypeText,
			},
			{
				ID:   "score",
				Name: "Score",
				Type: types.FieldTypeNumber,
			},
			{
				ID:   "severity",
				Name: "Severity",
				Type: types.FieldTypeSelect,
				Options: []config.FieldOption{
					{ID: "critical", Name: "Critical"},
					{ID: "high", Name: "High"},
					{ID: "medium", Name: "Medium"},
					{ID: "low", Name: "Low"},
				},
			},
			{
				ID:   "tags",
				Name: "Tags",
				Type: types.FieldTypeMultiSelect,
				Options: []config.FieldOption{
					{ID: "network", Name: "Network"},
					{ID: "malware", Name: "Malware"},
					{ID: "phishing", Name: "Phishing"},
				},
			},
			{
				ID:   "assignee",
				Name: "Assignee",
				Type: types.FieldTypeUser,
			},
			{
				ID:   "due-date",
				Name: "Due Date",
				Type: types.FieldTypeDate,
			},
			{
				ID:   "reference",
				Name: "Reference",
				Type: types.FieldTypeURL,
			},
			{
				ID:       "required-note",
				Name:     "Required Note",
				Type:     types.FieldTypeText,
				Required: true,
			},
		},
		Labels: config.EntityLabels{Case: "Case"},
	}
}

func setupValidateTest(t *testing.T, wsID string, schema *config.FieldSchema) (*memory.Memory, *usecase.UseCases) {
	t.Helper()
	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: wsID, Name: "Test Workspace"},
		FieldSchema: schema,
	})
	uc := usecase.New(repo, registry)
	return repo, uc
}

// newValidateTestStatusSet builds a status set with a single closed status, used
// for both the Action status and the thread-mode Case board status checks.
func newValidateTestStatusSet(t *testing.T) *model.ActionStatusSet {
	t.Helper()
	set, err := model.NewActionStatusSet("TRIAGE", []string{"DONE"}, []model.ActionStatusDefinition{
		{ID: "TRIAGE", Name: "Triage", Color: "idle"},
		{ID: "WORKING", Name: "Working", Color: "active"},
		{ID: "DONE", Name: "Done", Color: "success"},
	})
	gt.NoError(t, err).Required()
	return set
}

// issueByKind indexes issues so assertions can name the check they belong to.
func issueByKind(issues []usecase.ValidationIssue, kind usecase.ValidationIssueKind, fieldID string) (usecase.ValidationIssue, bool) {
	for _, issue := range issues {
		if issue.Kind == kind && issue.FieldID == fieldID {
			return issue, true
		}
	}
	return usecase.ValidationIssue{}, false
}

func TestValidateDB_NoCases(t *testing.T) {
	wsID := "ws-empty"
	_, uc := setupValidateTest(t, wsID, buildValidateTestSchema())

	result, err := uc.ValidateDB(context.Background())
	gt.NoError(t, err).Required()
	gt.Bool(t, result.HasIssues()).False()
}

func TestValidateDB_AllFieldTypesValid(t *testing.T) {
	wsID := "ws-valid"
	repo, uc := setupValidateTest(t, wsID, buildValidateTestSchema())
	ctx := context.Background()

	_, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID:  "U-TEST-DEFAULT",
		Title:       "Valid Case",
		Description: "All field types are valid",
		FieldValues: map[string]model.FieldValue{
			"title-text":    {FieldID: "title-text", Type: types.FieldTypeText, Value: "hello"},
			"score":         {FieldID: "score", Type: types.FieldTypeNumber, Value: float64(42)},
			"severity":      {FieldID: "severity", Type: types.FieldTypeSelect, Value: "high"},
			"tags":          {FieldID: "tags", Type: types.FieldTypeMultiSelect, Value: []string{"network", "malware"}},
			"assignee":      {FieldID: "assignee", Type: types.FieldTypeUser, Value: "U001"},
			"due-date":      {FieldID: "due-date", Type: types.FieldTypeDate, Value: "2026-02-14T00:00:00Z"},
			"reference":     {FieldID: "reference", Type: types.FieldTypeURL, Value: "https://example.com"},
			"required-note": {FieldID: "required-note", Type: types.FieldTypeText, Value: "noted"},
		},
	})
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Bool(t, result.HasIssues()).False()
}

func TestValidateDB_SelectInvalidOptionID(t *testing.T) {
	wsID := "ws-select-invalid"
	repo, uc := setupValidateTest(t, wsID, buildValidateTestSchema())
	ctx := context.Background()

	_, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Bad Select",
		FieldValues: map[string]model.FieldValue{
			"severity": {FieldID: "severity", Type: types.FieldTypeSelect, Value: "unknown-severity"},
		},
	})
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Array(t, result.Issues).Length(1).Required()
	gt.Value(t, result.Issues[0].Kind).Equal(usecase.IssueKindFieldValue)
	gt.Value(t, result.Issues[0].FieldID).Equal("severity")
	gt.Value(t, result.Issues[0].WorkspaceID).Equal(wsID)
	gt.Value(t, result.Issues[0].Actual).Equal("unknown-severity")
	gt.Value(t, result.Issues[0].Expected).Equal(string(types.FieldTypeSelect))
}

func TestValidateDB_TextSemanticViolation(t *testing.T) {
	wsID := "ws-semantic"
	schema := &config.FieldSchema{
		Fields: []config.FieldDefinition{
			{ID: "channel", Name: "Channel", Type: types.FieldTypeText, Semantic: types.SemanticSlackChannelID},
		},
		Labels: config.EntityLabels{Case: "Case"},
	}
	repo, uc := setupValidateTest(t, wsID, schema)
	ctx := context.Background()

	for _, v := range []string{"#general", "C0123ABCD", ""} {
		_, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID: "U-TEST-DEFAULT",
			Title:      "Channel " + v,
			FieldValues: map[string]model.FieldValue{
				"channel": {FieldID: "channel", Type: types.FieldTypeText, Value: v},
			},
		})
		gt.NoError(t, err).Required()
	}

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Array(t, result.Issues).Length(1).Required()
	gt.Value(t, result.Issues[0].Kind).Equal(usecase.IssueKindFieldValue)
	gt.Value(t, result.Issues[0].FieldID).Equal("channel")
	gt.Value(t, result.Issues[0].Actual).Equal("#general")
	gt.Value(t, result.Issues[0].Expected).Equal("text (slack_channel_id)")
}

func TestValidateDB_TextPatternViolation(t *testing.T) {
	wsID := "ws-pattern"
	schema := &config.FieldSchema{
		Fields: []config.FieldDefinition{
			{ID: "code", Name: "Code", Type: types.FieldTypeText,
				Validation: config.FieldValidation{Pattern: "^C[0-9]+$"}},
			{ID: "channel", Name: "Channel", Type: types.FieldTypeText,
				Semantic:   types.SemanticSlackChannelID,
				Validation: config.FieldValidation{Pattern: "^C[0-9]+$"}},
		},
		Labels: config.EntityLabels{Case: "Case"},
	}
	repo, uc := setupValidateTest(t, wsID, schema)
	ctx := context.Background()

	stored := []map[string]model.FieldValue{
		{"code": {FieldID: "code", Type: types.FieldTypeText, Value: "X1"}},
		{"code": {FieldID: "code", Type: types.FieldTypeText, Value: "C1"}},
		{"code": {FieldID: "code", Type: types.FieldTypeText, Value: ""}},
		{"channel": {FieldID: "channel", Type: types.FieldTypeText, Value: "C0123ABCD"}},
	}
	for i, fv := range stored {
		_, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID:  "U-TEST-DEFAULT",
			Title:       "Case " + strconv.Itoa(i),
			FieldValues: fv,
		})
		gt.NoError(t, err).Required()
	}

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Array(t, result.Issues).Length(2).Required()

	byField := map[string]usecase.ValidationIssue{}
	for _, issue := range result.Issues {
		byField[issue.FieldID] = issue
	}

	code := byField["code"]
	gt.Value(t, code.Kind).Equal(usecase.IssueKindFieldValue)
	gt.Value(t, code.Actual).Equal("X1")
	gt.Value(t, code.Expected).Equal("text (pattern=^C[0-9]+$)")

	channel := byField["channel"]
	gt.Value(t, channel.Kind).Equal(usecase.IssueKindFieldValue)
	gt.Value(t, channel.Actual).Equal("C0123ABCD")
	gt.Value(t, channel.Expected).Equal("text (slack_channel_id, pattern=^C[0-9]+$)")
}

func TestValidateDB_SelectWrongType(t *testing.T) {
	wsID := "ws-select-wrong-type"
	repo, uc := setupValidateTest(t, wsID, buildValidateTestSchema())
	ctx := context.Background()

	_, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Select Wrong Type",
		FieldValues: map[string]model.FieldValue{
			"severity": {FieldID: "severity", Type: types.FieldTypeSelect, Value: 42},
		},
	})
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Array(t, result.Issues).Length(1).Required()
	gt.Value(t, result.Issues[0].Kind).Equal(usecase.IssueKindFieldValue)
	gt.Value(t, result.Issues[0].FieldID).Equal("severity")
}

func TestValidateDB_MultiSelectInvalidOptionID(t *testing.T) {
	wsID := "ws-multiselect-invalid"
	repo, uc := setupValidateTest(t, wsID, buildValidateTestSchema())
	ctx := context.Background()

	_, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Bad MultiSelect",
		FieldValues: map[string]model.FieldValue{
			"tags": {FieldID: "tags", Type: types.FieldTypeMultiSelect, Value: []string{"network", "nonexistent"}},
		},
	})
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Array(t, result.Issues).Length(1).Required()
	gt.Value(t, result.Issues[0].Kind).Equal(usecase.IssueKindFieldValue)
	gt.Value(t, result.Issues[0].FieldID).Equal("tags")
}

func TestValidateDB_MultiSelectWrongType(t *testing.T) {
	wsID := "ws-multiselect-type"
	repo, uc := setupValidateTest(t, wsID, buildValidateTestSchema())
	ctx := context.Background()

	_, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Bad MultiSelect Type",
		FieldValues: map[string]model.FieldValue{
			"tags": {FieldID: "tags", Type: types.FieldTypeMultiSelect, Value: "should-be-array"},
		},
	})
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Array(t, result.Issues).Length(1).Required()
	gt.Value(t, result.Issues[0].FieldID).Equal("tags")
}

// TestValidateDB_NonSelectFieldTypesChecked pins the behaviour this feature added:
// before it, only select / multi-select values were inspected, so a text field
// holding a number or a date field holding a non-RFC3339 string went unreported.
func TestValidateDB_NonSelectFieldTypesChecked(t *testing.T) {
	wsID := "ws-non-select"
	repo, uc := setupValidateTest(t, wsID, buildValidateTestSchema())
	ctx := context.Background()

	_, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Non-select fields with wrong types",
		FieldValues: map[string]model.FieldValue{
			"title-text": {FieldID: "title-text", Type: types.FieldTypeText, Value: 12345},
			"score":      {FieldID: "score", Type: types.FieldTypeNumber, Value: "not-a-number"},
			"assignee":   {FieldID: "assignee", Type: types.FieldTypeUser, Value: 999},
			"due-date":   {FieldID: "due-date", Type: types.FieldTypeDate, Value: "14/02/2026"},
			"reference":  {FieldID: "reference", Type: types.FieldTypeURL, Value: 12345},
		},
	})
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Array(t, result.Issues).Length(5)

	for _, fieldID := range []string{"title-text", "score", "assignee", "due-date", "reference"} {
		issue, ok := issueByKind(result.Issues, usecase.IssueKindFieldValue, fieldID)
		gt.Bool(t, ok).True().Required()
		gt.Value(t, issue.Count).Equal(int64(1))
	}
}

func TestValidateDB_StoredFieldTypeMismatch(t *testing.T) {
	wsID := "ws-type-drift"
	schema := &config.FieldSchema{
		Fields: []config.FieldDefinition{
			{ID: "note", Name: "Note", Type: types.FieldTypeMarkdown},
		},
		Labels: config.EntityLabels{Case: "Case"},
	}
	repo, uc := setupValidateTest(t, wsID, schema)
	ctx := context.Background()

	// The value shape (string) is still acceptable for markdown, so only the
	// stored Type reveals that the schema used to declare this field as text.
	_, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Type drift",
		FieldValues: map[string]model.FieldValue{
			"note": {FieldID: "note", Type: types.FieldTypeText, Value: "still a string"},
		},
	})
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Array(t, result.Issues).Length(1).Required()
	gt.Value(t, result.Issues[0].Kind).Equal(usecase.IssueKindFieldTypeMismatch)
	gt.Value(t, result.Issues[0].FieldID).Equal("note")
	gt.Value(t, result.Issues[0].Expected).Equal(string(types.FieldTypeMarkdown))
	gt.Value(t, result.Issues[0].Actual).Equal(string(types.FieldTypeText))
}

func TestValidateDB_EmptyStoredFieldTypeNotReported(t *testing.T) {
	wsID := "ws-empty-type"
	schema := &config.FieldSchema{
		Fields: []config.FieldDefinition{
			{ID: "note", Name: "Note", Type: types.FieldTypeMarkdown},
		},
		Labels: config.EntityLabels{Case: "Case"},
	}
	repo, uc := setupValidateTest(t, wsID, schema)
	ctx := context.Background()

	// Documents written before FieldValue.Type was recorded carry an empty Type,
	// which says nothing about the schema and must not be reported as drift.
	_, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Legacy document",
		FieldValues: map[string]model.FieldValue{
			"note": {FieldID: "note", Value: "still a string"},
		},
	})
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Bool(t, result.HasIssues()).False()
}

func TestValidateDB_UnknownFieldSkipped(t *testing.T) {
	wsID := "ws-unknown-field"
	repo, uc := setupValidateTest(t, wsID, buildValidateTestSchema())
	ctx := context.Background()

	_, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Unknown Field",
		FieldValues: map[string]model.FieldValue{
			"unknown-field": {FieldID: "unknown-field", Type: types.FieldTypeSelect, Value: "anything"},
		},
	})
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Bool(t, result.HasIssues()).False()
}

// TestValidateDB_RequiredFieldNotReported pins the decision to leave required
// fields out of the consistency check: adding required=true to an existing
// workspace must not flag every Case that predates it.
func TestValidateDB_RequiredFieldNotReported(t *testing.T) {
	wsID := "ws-required"
	repo, uc := setupValidateTest(t, wsID, buildValidateTestSchema())
	ctx := context.Background()

	_, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID:  "U-TEST-DEFAULT",
		Title:       "Missing the required field",
		FieldValues: map[string]model.FieldValue{},
	})
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Bool(t, result.HasIssues()).False()
}

func TestValidateDB_MultipleIssuesAcrossFields(t *testing.T) {
	wsID := "ws-multi-issues"
	repo, uc := setupValidateTest(t, wsID, buildValidateTestSchema())
	ctx := context.Background()

	_, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Multiple Issues",
		FieldValues: map[string]model.FieldValue{
			"severity": {FieldID: "severity", Type: types.FieldTypeSelect, Value: "nonexistent"},
			"tags":     {FieldID: "tags", Type: types.FieldTypeMultiSelect, Value: []string{"bogus"}},
		},
	})
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Array(t, result.Issues).Length(2).Required()
	gt.Value(t, result.Issues[0].FieldID).Equal("severity")
	gt.Value(t, result.Issues[1].FieldID).Equal("tags")
}

func TestValidateDB_MultipleWorkspaces(t *testing.T) {
	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	ctx := context.Background()

	wsID1 := "ws-one"
	wsID2 := "ws-two"

	schema1 := &config.FieldSchema{
		Fields: []config.FieldDefinition{
			{
				ID: "status", Name: "Status", Type: types.FieldTypeSelect,
				Options: []config.FieldOption{
					{ID: "open", Name: "Open"},
					{ID: "closed", Name: "Closed"},
				},
			},
		},
		Labels: config.EntityLabels{Case: "Case"},
	}
	schema2 := &config.FieldSchema{
		Fields: []config.FieldDefinition{
			{
				ID: "priority", Name: "Priority", Type: types.FieldTypeSelect,
				Options: []config.FieldOption{
					{ID: "p0", Name: "P0"},
					{ID: "p1", Name: "P1"},
				},
			},
		},
		Labels: config.EntityLabels{Case: "Case"},
	}

	registry.Register(&model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: wsID1, Name: "Workspace 1"},
		FieldSchema: schema1,
	})
	registry.Register(&model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: wsID2, Name: "Workspace 2"},
		FieldSchema: schema2,
	})

	uc := usecase.New(repo, registry)

	_, err := repo.Case().Create(ctx, wsID1, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Valid in ws1",
		FieldValues: map[string]model.FieldValue{
			"status": {FieldID: "status", Type: types.FieldTypeSelect, Value: "open"},
		},
	})
	gt.NoError(t, err).Required()

	_, err = repo.Case().Create(ctx, wsID2, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Invalid in ws2",
		FieldValues: map[string]model.FieldValue{
			"priority": {FieldID: "priority", Type: types.FieldTypeSelect, Value: "p999"},
		},
	})
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Array(t, result.Issues).Length(1).Required()
	gt.Value(t, result.Issues[0].WorkspaceID).Equal(wsID2)
	gt.Value(t, result.Issues[0].FieldID).Equal("priority")
}

func TestValidateDB_InterfaceSliceMultiSelect(t *testing.T) {
	wsID := "ws-iface-slice"
	repo, uc := setupValidateTest(t, wsID, buildValidateTestSchema())
	ctx := context.Background()

	// []interface{} with valid strings — can happen from JSON/Firestore deserialization
	_, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Interface Slice MultiSelect",
		FieldValues: map[string]model.FieldValue{
			"tags": {FieldID: "tags", Type: types.FieldTypeMultiSelect, Value: []interface{}{"network", "malware"}},
		},
	})
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Bool(t, result.HasIssues()).False()
}

func TestValidateDB_InterfaceSliceMultiSelectInvalid(t *testing.T) {
	wsID := "ws-iface-slice-invalid"
	repo, uc := setupValidateTest(t, wsID, buildValidateTestSchema())
	ctx := context.Background()

	_, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Interface Slice Invalid",
		FieldValues: map[string]model.FieldValue{
			"tags": {FieldID: "tags", Type: types.FieldTypeMultiSelect, Value: []interface{}{123, 456}},
		},
	})
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Array(t, result.Issues).Length(1).Required()
	gt.Value(t, result.Issues[0].FieldID).Equal("tags")
}

func TestValidateDB_IssuesGroupedWithLowestSample(t *testing.T) {
	wsID := "ws-grouping"
	repo, uc := setupValidateTest(t, wsID, buildValidateTestSchema())
	ctx := context.Background()

	var firstID int64
	for i := range 3 {
		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID: "U-TEST-DEFAULT",
			Title:      "Bad Case",
			FieldValues: map[string]model.FieldValue{
				"severity": {FieldID: "severity", Type: types.FieldTypeSelect, Value: "removed"},
			},
		})
		gt.NoError(t, err).Required()
		if i == 0 {
			firstID = created.ID
		}
	}

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Array(t, result.Issues).Length(1).Required()

	issue := result.Issues[0]
	gt.Value(t, issue.Count).Equal(int64(3))
	gt.Value(t, issue.Sample.Kind).Equal(usecase.TargetKindCase)
	gt.Value(t, issue.Sample.CaseID).Equal(firstID)
	gt.Value(t, issue.Sample.String()).Equal("case:" + strconv.FormatInt(firstID, 10))
	gt.Value(t, issue.Actual).Equal("removed")
	gt.Value(t, result.TotalCount()).Equal(int64(3))
}

func TestValidateDB_ResultIsDeterministic(t *testing.T) {
	wsID := "ws-deterministic"
	repo, uc := setupValidateTest(t, wsID, buildValidateTestSchema())
	ctx := context.Background()

	for range 4 {
		_, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID: "U-TEST-DEFAULT",
			Title:      "Bad Case",
			FieldValues: map[string]model.FieldValue{
				"severity": {FieldID: "severity", Type: types.FieldTypeSelect, Value: "removed"},
				"tags":     {FieldID: "tags", Type: types.FieldTypeMultiSelect, Value: []string{"gone"}},
				"score":    {FieldID: "score", Type: types.FieldTypeNumber, Value: "not-a-number"},
			},
		})
		gt.NoError(t, err).Required()
	}

	first, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	second, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Value(t, second.Issues).Equal(first.Issues)

	// Sorted by (workspace, kind, field id).
	gt.Array(t, first.Issues).Length(3).Required()
	gt.Value(t, first.Issues[0].FieldID).Equal("score")
	gt.Value(t, first.Issues[1].FieldID).Equal("severity")
	gt.Value(t, first.Issues[2].FieldID).Equal("tags")
}

func TestValidateDB_CaseRefMissingTarget(t *testing.T) {
	refWS := "ws-ref-target"
	wsID := "ws-ref-source"
	ctx := context.Background()

	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace: model.Workspace{ID: refWS, Name: "Reference Workspace"},
		FieldSchema: &config.FieldSchema{
			Fields: []config.FieldDefinition{{ID: "note", Name: "Note", Type: types.FieldTypeText}},
			Labels: config.EntityLabels{Case: "Case"},
		},
	})
	registry.Register(&model.WorkspaceEntry{
		Workspace: model.Workspace{ID: wsID, Name: "Source Workspace"},
		FieldSchema: &config.FieldSchema{
			Fields: []config.FieldDefinition{
				{ID: "parent", Name: "Parent", Type: types.FieldTypeCaseRef, ReferenceWorkspace: refWS},
			},
			Labels: config.EntityLabels{Case: "Case"},
		},
	})
	uc := usecase.New(repo, registry)

	target, err := repo.Case().Create(ctx, refWS, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Referenced case",
	})
	gt.NoError(t, err).Required()

	_, err = repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Points at an existing case",
		FieldValues: map[string]model.FieldValue{
			"parent": {FieldID: "parent", Type: types.FieldTypeCaseRef, Value: strconv.FormatInt(target.ID, 10)},
		},
	})
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Bool(t, result.HasIssues()).False()

	dangling, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Points at a missing case",
		FieldValues: map[string]model.FieldValue{
			"parent": {FieldID: "parent", Type: types.FieldTypeCaseRef, Value: "99999"},
		},
	})
	gt.NoError(t, err).Required()

	result, err = uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Array(t, result.Issues).Length(1).Required()
	gt.Value(t, result.Issues[0].Kind).Equal(usecase.IssueKindCaseRefMissing)
	gt.Value(t, result.Issues[0].FieldID).Equal("parent")
	gt.Value(t, result.Issues[0].WorkspaceID).Equal(wsID)
	gt.Value(t, result.Issues[0].Sample.CaseID).Equal(dangling.ID)
	gt.Value(t, result.Issues[0].Actual).Equal("99999")
}

// TestValidateDB_MultiCaseRefCountsEntitiesNotReferences pins that Count is the
// number of affected entities: one multi_case_ref value carrying several missing
// ids — including the same id twice — is still one Case.
func TestValidateDB_MultiCaseRefCountsEntitiesNotReferences(t *testing.T) {
	refWS := "ws-multiref-target"
	wsID := "ws-multiref-source"
	ctx := context.Background()

	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace: model.Workspace{ID: refWS, Name: "Reference Workspace"},
		FieldSchema: &config.FieldSchema{
			Fields: []config.FieldDefinition{{ID: "note", Name: "Note", Type: types.FieldTypeText}},
			Labels: config.EntityLabels{Case: "Case"},
		},
	})
	registry.Register(&model.WorkspaceEntry{
		Workspace: model.Workspace{ID: wsID, Name: "Source Workspace"},
		FieldSchema: &config.FieldSchema{
			Fields: []config.FieldDefinition{
				{ID: "related", Name: "Related", Type: types.FieldTypeMultiCaseRef, ReferenceWorkspace: refWS},
			},
			Labels: config.EntityLabels{Case: "Case"},
		},
	})
	uc := usecase.New(repo, registry)

	present, err := repo.Case().Create(ctx, refWS, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Referenced case",
	})
	gt.NoError(t, err).Required()

	source, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Two missing ids and one duplicate",
		FieldValues: map[string]model.FieldValue{
			"related": {
				FieldID: "related",
				Type:    types.FieldTypeMultiCaseRef,
				Value: []string{
					strconv.FormatInt(present.ID, 10),
					"77777",
					"88888",
					"77777",
				},
			},
		},
	})
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Array(t, result.Issues).Length(1).Required()

	issue := result.Issues[0]
	gt.Value(t, issue.Kind).Equal(usecase.IssueKindCaseRefMissing)
	gt.Value(t, issue.FieldID).Equal("related")
	gt.Value(t, issue.Count).Equal(int64(1))
	gt.Value(t, issue.Sample.CaseID).Equal(source.ID)
	// Every missing id of that one entity, ascending, deduplicated.
	gt.Value(t, issue.Actual).Equal("77777,88888")
	gt.Value(t, result.TotalCount()).Equal(int64(1))
}

func TestValidateDB_BoardStatusInvalid(t *testing.T) {
	wsID := "ws-board-status"
	ctx := context.Background()

	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace:     model.Workspace{ID: wsID, Name: "Thread Workspace"},
		FieldSchema:   &config.FieldSchema{Labels: config.EntityLabels{Case: "Case"}},
		CaseMode:      model.CaseModeThread,
		CaseStatusSet: newValidateTestStatusSet(t),
	})
	uc := usecase.New(repo, registry)

	undefined, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID:     "U-TEST-DEFAULT",
		Title:          "Board status dropped from config",
		Status:         types.CaseStatusOpen,
		SlackChannelID: "C-MONITOR",
		SlackThreadTS:  "1700000000.000100",
		BoardStatus:    "RETIRED",
	})
	gt.NoError(t, err).Required()

	_, err = repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID:     "U-TEST-DEFAULT",
		Title:          "No board status at all",
		Status:         types.CaseStatusOpen,
		SlackChannelID: "C-MONITOR",
		SlackThreadTS:  "1700000000.000200",
	})
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Array(t, result.Issues).Length(1).Required()

	issue := result.Issues[0]
	gt.Value(t, issue.Kind).Equal(usecase.IssueKindBoardStatus)
	gt.Value(t, issue.FieldID).Equal("")
	gt.Value(t, issue.Count).Equal(int64(2))
	// Both offenders collapse into one group; the lower case id wins the sample.
	gt.Value(t, issue.Sample.CaseID).Equal(undefined.ID)
	gt.Value(t, issue.Actual).Equal("RETIRED")
}

func TestValidateDB_ArchivedCaseNotClosed(t *testing.T) {
	wsID := "ws-archived-not-closed"
	repo, uc := setupValidateTest(t, wsID, &config.FieldSchema{Labels: config.EntityLabels{Case: "Case"}})
	ctx := context.Background()

	archivedAt := time.Now().UTC()

	// The offender: archived but still OPEN. Unreachable through the usecase
	// layer, but a Case in this state appears in no listing at all, so the
	// check is what surfaces it.
	offender, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Archived but open",
		Status:     types.CaseStatusOpen,
		ArchivedAt: &archivedAt,
	})
	gt.NoError(t, err).Required()

	// A correctly archived case and an ordinary open case must not be flagged.
	_, err = repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Archived and closed",
		Status:     types.CaseStatusClosed,
		ArchivedAt: &archivedAt,
	})
	gt.NoError(t, err).Required()

	_, err = repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Open and active",
		Status:     types.CaseStatusOpen,
	})
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Array(t, result.Issues).Length(1).Required()

	issue := result.Issues[0]
	gt.Value(t, issue.Kind).Equal(usecase.IssueKindArchivedNotClosed)
	gt.Value(t, issue.FieldID).Equal("")
	gt.Value(t, issue.Count).Equal(int64(1))
	gt.Value(t, issue.Sample.CaseID).Equal(offender.ID)
	gt.Value(t, issue.Expected).Equal(string(types.CaseStatusClosed))
	gt.Value(t, issue.Actual).Equal(string(types.CaseStatusOpen))
}

func TestValidateDB_ArchivedCaseClosedIsClean(t *testing.T) {
	wsID := "ws-archived-clean"
	repo, uc := setupValidateTest(t, wsID, &config.FieldSchema{Labels: config.EntityLabels{Case: "Case"}})
	ctx := context.Background()

	archivedAt := time.Now().UTC()
	_, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Archived and closed",
		Status:     types.CaseStatusClosed,
		ArchivedAt: &archivedAt,
	})
	gt.NoError(t, err).Required()

	_, err = repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Open and active",
		Status:     types.CaseStatusOpen,
	})
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Array(t, result.Issues).Length(0)
}

func TestValidateDB_BoardStatusSkippedForChannelMode(t *testing.T) {
	wsID := "ws-channel-mode"
	repo, uc := setupValidateTest(t, wsID, &config.FieldSchema{Labels: config.EntityLabels{Case: "Case"}})
	ctx := context.Background()

	// No CaseStatusSet registered: a channel-mode Case legitimately has no board
	// status, so nothing here may be reported.
	_, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID:     "U-TEST-DEFAULT",
		Title:          "Channel-mode case",
		Status:         types.CaseStatusOpen,
		SlackChannelID: "C-DEDICATED",
	})
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Bool(t, result.HasIssues()).False()
}

// TestValidateDB_BoardStatusSkippedWhenModeIsChannel covers a workspace switched
// to channel mode whose TOML still carries a [case] section: config builds a
// CaseStatusSet from that section alone, so the status set is present even though
// the workspace no longer uses a board. Old thread-bound Cases must not be
// checked against it.
func TestValidateDB_BoardStatusSkippedWhenModeIsChannel(t *testing.T) {
	wsID := "ws-mode-switched-to-channel"
	ctx := context.Background()

	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace:     model.Workspace{ID: wsID, Name: "Switched Workspace"},
		FieldSchema:   &config.FieldSchema{Labels: config.EntityLabels{Case: "Case"}},
		CaseMode:      model.CaseModeChannel,
		CaseStatusSet: newValidateTestStatusSet(t),
	})
	uc := usecase.New(repo, registry)

	// Both a board status the set does not define and a closed/open divergence:
	// neither may be reported while the workspace is in channel mode.
	_, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID:     "U-TEST-DEFAULT",
		Title:          "Left over from thread mode",
		Status:         types.CaseStatusOpen,
		SlackChannelID: "C-MONITOR",
		SlackThreadTS:  "1700000000.000500",
		BoardStatus:    "RETIRED",
	})
	gt.NoError(t, err).Required()

	_, err = repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID:     "U-TEST-DEFAULT",
		Title:          "Closed board status still open",
		Status:         types.CaseStatusOpen,
		SlackChannelID: "C-MONITOR",
		SlackThreadTS:  "1700000000.000600",
		BoardStatus:    "DONE",
	})
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Bool(t, result.HasIssues()).False()
}

// TestValidateDB_BoardStatusSkippedForDraft pins that a thread-mode DRAFT is out
// of scope for both status checks: a draft is an unfinished entry, not divergent
// data.
func TestValidateDB_BoardStatusSkippedForDraft(t *testing.T) {
	wsID := "ws-thread-draft"
	ctx := context.Background()

	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace:     model.Workspace{ID: wsID, Name: "Thread Workspace"},
		FieldSchema:   &config.FieldSchema{Labels: config.EntityLabels{Case: "Case"}},
		CaseMode:      model.CaseModeThread,
		CaseStatusSet: newValidateTestStatusSet(t),
	})
	uc := usecase.New(repo, registry)

	_, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID:     "U-TEST-DEFAULT",
		Title:          "Draft with a dropped board status",
		Status:         types.CaseStatusDraft,
		SlackChannelID: "C-MONITOR",
		SlackThreadTS:  "1700000000.000700",
		BoardStatus:    "RETIRED",
	})
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Bool(t, result.HasIssues()).False()
}

// TestValidateDB_BoardStatusSkippedForChannelBoundCase covers a Case created
// before the workspace moved to thread mode: it is channel-bound, and
// applyThreadBinding — the only writer of BoardStatus — never runs for it, so an
// empty BoardStatus there is expected rather than divergent.
func TestValidateDB_BoardStatusSkippedForChannelBoundCase(t *testing.T) {
	wsID := "ws-thread-with-legacy-case"
	ctx := context.Background()

	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace:     model.Workspace{ID: wsID, Name: "Thread Workspace"},
		FieldSchema:   &config.FieldSchema{Labels: config.EntityLabels{Case: "Case"}},
		CaseMode:      model.CaseModeThread,
		CaseStatusSet: newValidateTestStatusSet(t),
	})
	uc := usecase.New(repo, registry)

	_, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID:     "U-TEST-DEFAULT",
		Title:          "Channel-bound case predating the mode switch",
		Status:         types.CaseStatusOpen,
		SlackChannelID: "C-DEDICATED",
	})
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Bool(t, result.HasIssues()).False()
}

func TestValidateDB_LifecycleStatusMismatch(t *testing.T) {
	wsID := "ws-lifecycle"
	ctx := context.Background()

	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace:     model.Workspace{ID: wsID, Name: "Thread Workspace"},
		FieldSchema:   &config.FieldSchema{Labels: config.EntityLabels{Case: "Case"}},
		CaseMode:      model.CaseModeThread,
		CaseStatusSet: newValidateTestStatusSet(t),
	})
	uc := usecase.New(repo, registry)

	// DONE is configured as closed, so the lifecycle status should be CLOSED.
	diverged, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID:     "U-TEST-DEFAULT",
		Title:          "Closed board status but still open",
		Status:         types.CaseStatusOpen,
		SlackChannelID: "C-MONITOR",
		SlackThreadTS:  "1700000000.000300",
		BoardStatus:    "DONE",
	})
	gt.NoError(t, err).Required()

	_, err = repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID:     "U-TEST-DEFAULT",
		Title:          "Consistent pair",
		Status:         types.CaseStatusOpen,
		SlackChannelID: "C-MONITOR",
		SlackThreadTS:  "1700000000.000400",
		BoardStatus:    "WORKING",
	})
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Array(t, result.Issues).Length(1).Required()

	issue := result.Issues[0]
	gt.Value(t, issue.Kind).Equal(usecase.IssueKindLifecycleMismatch)
	gt.Value(t, issue.Count).Equal(int64(1))
	gt.Value(t, issue.Sample.CaseID).Equal(diverged.ID)
	gt.Value(t, issue.Expected).Equal(string(types.CaseStatusClosed))
	gt.Value(t, issue.Actual).Equal(string(types.CaseStatusOpen))
}

func TestValidateDB_ActionStatusInvalid(t *testing.T) {
	wsID := "ws-action-status"
	ctx := context.Background()

	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace:       model.Workspace{ID: wsID, Name: "Action Workspace"},
		FieldSchema:     &config.FieldSchema{Labels: config.EntityLabels{Case: "Case"}},
		ActionStatusSet: newValidateTestStatusSet(t),
	})
	uc := usecase.New(repo, registry)

	parent, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Parent case",
	})
	gt.NoError(t, err).Required()

	_, err = repo.Action().Create(ctx, wsID, &model.Action{
		CaseID: parent.ID,
		Title:  "Valid status",
		Status: types.ActionStatus("WORKING"),
	})
	gt.NoError(t, err).Required()

	bad, err := repo.Action().Create(ctx, wsID, &model.Action{
		CaseID: parent.ID,
		Title:  "Status dropped from config",
		Status: types.ActionStatus("ABANDONED"),
	})
	gt.NoError(t, err).Required()

	archivedAt := bad.CreatedAt
	archived, err := repo.Action().Create(ctx, wsID, &model.Action{
		CaseID:     parent.ID,
		Title:      "Archived with a dropped status",
		Status:     types.ActionStatus("ABANDONED"),
		ArchivedAt: &archivedAt,
	})
	gt.NoError(t, err).Required()
	gt.Bool(t, archived.IsArchived()).True()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Array(t, result.Issues).Length(1).Required()

	issue := result.Issues[0]
	gt.Value(t, issue.Kind).Equal(usecase.IssueKindActionStatus)
	// Archived actions count too: they remain visible in the Case history.
	gt.Value(t, issue.Count).Equal(int64(2))
	gt.Value(t, issue.Sample.Kind).Equal(usecase.TargetKindAction)
	gt.Value(t, issue.Sample.CaseID).Equal(parent.ID)
	gt.Value(t, issue.Sample.ActionID).Equal(bad.ID)
	gt.Value(t, issue.Actual).Equal("ABANDONED")
}

// An Action stored in a thread-mode workspace that does not enable Actions is
// shown nowhere in the UI, so it is reported; turning [case] actions on clears
// the report.
func TestValidateDB_ActionsDisabled(t *testing.T) {
	ctx := context.Background()
	for name, enabled := range map[string]bool{"actions off": false, "actions on": true} {
		t.Run(name, func(t *testing.T) {
			wsID := "ws-thread-actions"
			repo := memory.New()
			registry := model.NewWorkspaceRegistry()
			registry.Register(&model.WorkspaceEntry{
				Workspace:       model.Workspace{ID: wsID, Name: "Thread Workspace"},
				FieldSchema:     &config.FieldSchema{Labels: config.EntityLabels{Case: "Case"}},
				ActionStatusSet: newValidateTestStatusSet(t),
				CaseMode:        model.CaseModeThread,
				CaseStatusSet:   newValidateTestStatusSet(t),
				CaseActions:     enabled,
			})
			uc := usecase.New(repo, registry)

			parent, err := repo.Case().Create(ctx, wsID, &model.Case{
				Title:          "Thread case",
				Status:         types.CaseStatusOpen,
				SlackChannelID: "C-MONITOR",
				SlackThreadTS:  "1700000000.000100",
				BoardStatus:    "TRIAGE",
			})
			gt.NoError(t, err).Required()
			first, err := repo.Action().Create(ctx, wsID, &model.Action{
				CaseID: parent.ID, Title: "First action", Status: types.ActionStatus("TRIAGE"),
			})
			gt.NoError(t, err).Required()
			_, err = repo.Action().Create(ctx, wsID, &model.Action{
				CaseID: parent.ID, Title: "Second action", Status: types.ActionStatus("WORKING"),
			})
			gt.NoError(t, err).Required()

			result, err := uc.ValidateDB(ctx)
			gt.NoError(t, err).Required()
			if enabled {
				gt.Array(t, result.Issues).Length(0)
				return
			}
			gt.Array(t, result.Issues).Length(1).Required()
			issue := result.Issues[0]
			gt.Value(t, issue.Kind).Equal(usecase.IssueKindActionsDisabled)
			gt.Value(t, issue.Count).Equal(int64(2))
			gt.Value(t, issue.Sample.Kind).Equal(usecase.TargetKindAction)
			gt.Value(t, issue.Sample.CaseID).Equal(parent.ID)
			gt.Value(t, issue.Sample.ActionID).Equal(first.ID)
		})
	}
}

func TestValidateDB_MemoFieldValues(t *testing.T) {
	wsID := "ws-memo"
	ctx := context.Background()

	memoSchema := &config.FieldSchema{
		Fields: []config.FieldDefinition{
			{
				ID: "kind", Name: "Kind", Type: types.FieldTypeSelect,
				Options: []config.FieldOption{{ID: "fact", Name: "Fact"}},
			},
		},
		Labels: config.EntityLabels{Case: "Case"},
	}

	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: wsID, Name: "Memo Workspace"},
		FieldSchema: &config.FieldSchema{Labels: config.EntityLabels{Case: "Case"}},
		MemoConfig:  &config.MemoConfig{Description: "notes", FieldSchema: memoSchema},
	})
	uc := usecase.New(repo, registry)

	parent, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Parent case",
	})
	gt.NoError(t, err).Required()

	badMemo := &model.Memo{
		ID:          model.NewMemoID(),
		WorkspaceID: wsID,
		CaseID:      parent.ID,
		Title:       "Memo with a dropped option",
		CreatorID:   "U-TEST-DEFAULT",
		FieldValues: map[string]model.FieldValue{
			"kind": {FieldID: "kind", Type: types.FieldTypeSelect, Value: "hypothesis"},
		},
	}
	_, err = repo.Memo().Create(ctx, wsID, badMemo)
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Array(t, result.Issues).Length(1).Required()

	issue := result.Issues[0]
	gt.Value(t, issue.Kind).Equal(usecase.IssueKindFieldValue)
	gt.Value(t, issue.FieldID).Equal("kind")
	gt.Value(t, issue.Sample.Kind).Equal(usecase.TargetKindMemo)
	gt.Value(t, issue.Sample.CaseID).Equal(parent.ID)
	gt.Value(t, issue.Sample.MemoID).Equal(badMemo.ID)
	gt.Value(t, issue.Actual).Equal("hypothesis")
}

func TestValidateDB_MemoSkippedWhenDisabled(t *testing.T) {
	wsID := "ws-memo-disabled"
	repo, uc := setupValidateTest(t, wsID, &config.FieldSchema{Labels: config.EntityLabels{Case: "Case"}})
	ctx := context.Background()

	parent, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Parent case",
	})
	gt.NoError(t, err).Required()

	// No MemoConfig registered: the memo pass must not run at all, even though a
	// memo document exists with a value no schema could accept.
	_, err = repo.Memo().Create(ctx, wsID, &model.Memo{
		ID:          model.NewMemoID(),
		WorkspaceID: wsID,
		CaseID:      parent.ID,
		Title:       "Orphan memo",
		CreatorID:   "U-TEST-DEFAULT",
		FieldValues: map[string]model.FieldValue{
			"kind": {FieldID: "kind", Type: types.FieldTypeSelect, Value: "hypothesis"},
		},
	})
	gt.NoError(t, err).Required()

	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Bool(t, result.HasIssues()).False()
}

// scanFailureRepository substitutes a Case repository whose ScanAll always
// fails, leaving every other repository backed by the in-memory implementation.
type scanFailureRepository struct {
	*memory.Memory
	caseRepo interfaces.CaseRepository
}

func (r *scanFailureRepository) Case() interfaces.CaseRepository { return r.caseRepo }

type scanFailureCaseRepository struct {
	interfaces.CaseRepository
	err error
}

func (r *scanFailureCaseRepository) ScanAll(_ context.Context, _ string, _ func(*model.Case) error) error {
	return r.err
}

func TestValidateDB_PropagatesRepositoryError(t *testing.T) {
	wsID := "ws-scan-error"
	ctx := context.Background()

	base := memory.New()
	scanErr := errors.New("scan exploded")
	repo := &scanFailureRepository{
		Memory:   base,
		caseRepo: &scanFailureCaseRepository{CaseRepository: base.Case(), err: scanErr},
	}

	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: wsID, Name: "Broken Workspace"},
		FieldSchema: buildValidateTestSchema(),
	})
	uc := usecase.New(repo, registry)

	result, err := uc.ValidateDB(ctx)
	gt.Value(t, result).Nil()
	gt.Error(t, err).Is(scanErr)
}

// TestValidateDBWithConfig_UsesSuppliedRegistry pins that the caller's
// configuration decides the verdict, not the one this UseCases was built with:
// the same stored value is clean under the process schema and reported under the
// supplied one. This is what the HTTP check endpoint relies on.
func TestValidateDBWithConfig_UsesSuppliedRegistry(t *testing.T) {
	wsID := "ws-supplied-config"
	repo, uc := setupValidateTest(t, wsID, buildValidateTestSchema())
	ctx := context.Background()

	stored, err := repo.Case().Create(ctx, wsID, &model.Case{
		ReporterID: "U-TEST-DEFAULT",
		Title:      "Severity is high",
		FieldValues: map[string]model.FieldValue{
			"severity": {FieldID: "severity", Type: types.FieldTypeSelect, Value: "high"},
		},
	})
	gt.NoError(t, err).Required()

	// The process configuration lists "high", so nothing is wrong today.
	result, err := uc.ValidateDB(ctx)
	gt.NoError(t, err).Required()
	gt.Bool(t, result.HasIssues()).False()

	// A candidate configuration that drops the option reports the stored value.
	candidate := model.NewWorkspaceRegistry()
	candidate.Register(&model.WorkspaceEntry{
		Workspace: model.Workspace{ID: wsID, Name: "Candidate Config"},
		FieldSchema: &config.FieldSchema{
			Fields: []config.FieldDefinition{
				{
					ID:      "severity",
					Name:    "Severity",
					Type:    types.FieldTypeSelect,
					Options: []config.FieldOption{{ID: "critical", Name: "Critical"}},
				},
			},
			Labels: config.EntityLabels{Case: "Case"},
		},
	})

	result, err = uc.ValidateDBWithConfig(ctx, candidate)
	gt.NoError(t, err).Required()
	gt.Array(t, result.Issues).Length(1).Required()
	gt.Value(t, result.Issues[0].Kind).Equal(usecase.IssueKindFieldValue)
	gt.Value(t, result.Issues[0].FieldID).Equal("severity")
	gt.Value(t, result.Issues[0].WorkspaceID).Equal(wsID)
	gt.Value(t, result.Issues[0].Actual).Equal("high")
	gt.Number(t, result.Issues[0].Sample.CaseID).Equal(stored.ID)
}

func TestValidateDBWithConfig_NilRegistryIsAnError(t *testing.T) {
	_, uc := setupValidateTest(t, "ws-nil-registry", buildValidateTestSchema())

	result, err := uc.ValidateDBWithConfig(context.Background(), nil)
	gt.Value(t, result).Nil()
	gt.Value(t, err).NotNil()
}
