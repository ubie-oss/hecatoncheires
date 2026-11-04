package usecase_test

import (
	"context"
	"testing"

	"github.com/m-mizutani/gt"
	goslack "github.com/slack-go/slack" //nolint:depguard

	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase"
)

func TestRenderSummaryFields_Semantic(t *testing.T) {
	entry := &model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "ws"},
		FieldSchema: &config.FieldSchema{Fields: []config.FieldDefinition{
			{ID: "channel", Name: "Notify", Type: types.FieldTypeText, Semantic: types.SemanticSlackChannelID},
			{ID: "legacy", Name: "Legacy", Type: types.FieldTypeText, Semantic: types.SemanticSlackChannelID},
			{ID: "note", Name: "Note", Type: types.FieldTypeText},
		}},
	}
	c := &model.Case{FieldValues: map[string]model.FieldValue{
		"channel": {FieldID: "channel", Type: types.FieldTypeText, Value: "C0123ABCD"},
		"legacy":  {FieldID: "legacy", Type: types.FieldTypeText, Value: "#old"},
		"note":    {FieldID: "note", Type: types.FieldTypeText, Value: "C0123ABCD"},
	}}

	gt.Equal(t, usecase.RenderSummaryFieldsForTest(c, entry), []string{
		"• *Notify*: <#C0123ABCD>",
		// A stored value that does not fit the semantic keeps its raw rendering.
		"• *Legacy*: #old",
		"• *Note*: C0123ABCD",
	})
}

func TestBuildThreadCaseSummaryBlocks_Style(t *testing.T) {
	const url = "https://hc.example.com/ws/ws/cases/1"
	newEntry := func(style model.CaseSummaryStyle) *model.WorkspaceEntry {
		return &model.WorkspaceEntry{
			Workspace:   model.Workspace{ID: "ws"},
			CaseSummary: style,
			FieldSchema: &config.FieldSchema{Fields: []config.FieldDefinition{
				{ID: "note", Name: "Note", Type: types.FieldTypeText},
			}},
		}
	}
	c := &model.Case{
		Title:       "Sample case <1> | A&B",
		Description: "Something broke",
		FieldValues: map[string]model.FieldValue{
			"note": {FieldID: "note", Type: types.FieldTypeText, Value: "n1"},
		},
	}
	sectionText := func(t *testing.T, b goslack.Block) string {
		t.Helper()
		sec, ok := b.(*goslack.SectionBlock)
		gt.True(t, ok).Required()
		return sec.Text.Text
	}

	for name, style := range map[string]model.CaseSummaryStyle{
		"unset defaults to the full summary": "",
		"full keeps the detailed summary":    model.CaseSummaryFull,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := i18n.ContextWithLang(context.Background(), i18n.LangEN)
			blocks, fallback := usecase.BuildThreadCaseSummaryBlocksForTest(ctx, c, newEntry(style), url)

			gt.A(t, blocks).Length(4).Required()
			_, isHeader := blocks[0].(*goslack.HeaderBlock)
			gt.True(t, isHeader)
			gt.Equal(t, sectionText(t, blocks[1]),
				"*Title*\nSample case <1> | A&B\n\n*Description*\nSomething broke")
			gt.Equal(t, sectionText(t, blocks[2]), "• *Note*: n1")
			_, isContext := blocks[3].(*goslack.ContextBlock)
			gt.True(t, isContext)
			gt.Equal(t, fallback, ":white_check_mark: Created a case — Sample case <1> | A&B "+url)
		})
	}

	t.Run("link renders one linked line (ja)", func(t *testing.T) {
		ctx := i18n.ContextWithLang(context.Background(), i18n.LangJA)
		blocks, fallback := usecase.BuildThreadCaseSummaryBlocksForTest(ctx, c, newEntry(model.CaseSummaryLink), url)

		gt.A(t, blocks).Length(1).Required()
		gt.Equal(t, sectionText(t, blocks[0]),
			":white_check_mark: ケースを作成しました: <"+url+"|Sample case &lt;1&gt; / A&amp;B>")
		gt.Equal(t, fallback, ":white_check_mark: ケースを作成しました — Sample case <1> | A&B "+url)
	})

	t.Run("link renders one linked line (en)", func(t *testing.T) {
		ctx := i18n.ContextWithLang(context.Background(), i18n.LangEN)
		blocks, _ := usecase.BuildThreadCaseSummaryBlocksForTest(ctx, &model.Case{Title: "Sample case 1"}, newEntry(model.CaseSummaryLink), url)

		gt.A(t, blocks).Length(1).Required()
		gt.Equal(t, sectionText(t, blocks[0]),
			":white_check_mark: Created a case: <"+url+"|Sample case 1>")
	})

	t.Run("link without a base URL shows the plain title", func(t *testing.T) {
		ctx := i18n.ContextWithLang(context.Background(), i18n.LangEN)
		blocks, fallback := usecase.BuildThreadCaseSummaryBlocksForTest(ctx, &model.Case{Title: "Sample case 1"}, newEntry(model.CaseSummaryLink), "")

		gt.A(t, blocks).Length(1).Required()
		gt.Equal(t, sectionText(t, blocks[0]), ":white_check_mark: Created a case: Sample case 1")
		gt.Equal(t, fallback, ":white_check_mark: Created a case — Sample case 1")
	})
}
