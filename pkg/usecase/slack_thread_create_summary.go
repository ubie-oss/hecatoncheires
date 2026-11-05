package usecase

import (
	"context"
	"fmt"
	"strings"

	"github.com/m-mizutani/goerr/v2"
	goslack "github.com/slack-go/slack" //nolint:depguard

	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/semantic"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
	"github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/errutil"
)

// replaceRootWithCaseSummary replaces an existing thread-root message with the
// shared Block Kit case summary, in place. It is best-effort and never returns
// an error: the case is already persisted, so a Slack render failure must not
// fail the caller. If the Block Kit update fails (e.g. block validation /
// payload size) it retries the same root with plain text so the anchor is never
// left on a stale placeholder; only if that also fails does it fall back to a
// threaded reply so the summary is never lost.
//
// This is the single source of the summary-replacement retry policy, shared by
// the reaction cross-channel path (AgentUseCase.updateRootCaseSummary) and the
// thread-mode web / slash / mention creation path
// (CaseUseCase.createThreadModeCase).
func replaceRootWithCaseSummary(ctx context.Context, svc slack.Service, entry *model.WorkspaceEntry, c *model.Case, channelID, rootTS, url string) {
	if svc == nil {
		return
	}
	blocks, fallback := buildThreadCaseSummaryBlocks(ctx, c, entry, url)
	if err := svc.UpdateMessage(ctx, channelID, rootTS, blocks, fallback); err != nil {
		errutil.Handle(ctx, goerr.Wrap(err, "update root case summary with blocks",
			goerr.V("channel_id", channelID), goerr.V("root_ts", rootTS)), "update root case summary with blocks")
		// Retry with plain text (no blocks) so the placeholder root is not left
		// stuck; only fall back to a threaded reply if that also fails.
		if txtErr := svc.UpdateMessage(ctx, channelID, rootTS, nil, fallback); txtErr != nil {
			errutil.Handle(ctx, goerr.Wrap(txtErr, "update root case summary with text",
				goerr.V("channel_id", channelID), goerr.V("root_ts", rootTS)), "update root case summary with text")
			if _, rErr := svc.PostThreadReply(ctx, channelID, rootTS, fallback); rErr != nil {
				errutil.Handle(ctx, goerr.Wrap(rErr, "post root case summary as reply",
					goerr.V("channel_id", channelID), goerr.V("root_ts", rootTS)), "post root case summary as reply")
			}
		}
	}
}

// buildThreadCaseSummaryBlocks renders the Block Kit summary posted to the
// thread once the initialization (create) agent commits a new case. By default
// it shows the title, description, the populated custom fields (resolved to
// option names), the board status, and a web-UI link; a workspace with
// [slack] case_summary = "link" gets a single linked line instead. The
// fallback string is used by Slack clients that cannot render blocks.
func buildThreadCaseSummaryBlocks(ctx context.Context, c *model.Case, entry *model.WorkspaceEntry, url string) ([]goslack.Block, string) {
	header := i18n.T(ctx, i18n.MsgThreadCaseSummaryHeader)
	fallback := header + " — " + orDash(c.Title)
	if url != "" {
		fallback += " " + url
	}

	if entry != nil && entry.CaseSummary.IsLink() {
		return []goslack.Block{buildThreadCaseSummaryLinkBlock(ctx, c, header, url)}, fallback
	}

	titleLabel := i18n.T(ctx, i18n.MsgThreadCaseSummaryTitle)
	descLabel := i18n.T(ctx, i18n.MsgThreadCaseSummaryDesc)
	statusLabel := i18n.T(ctx, i18n.MsgThreadCaseSummaryStatus)

	blocks := []goslack.Block{
		goslack.NewHeaderBlock(goslack.NewTextBlockObject(goslack.PlainTextType, header, true, false)),
	}

	body := fmt.Sprintf("*%s*\n%s", titleLabel, orDash(c.Title))
	if c.Description != "" {
		body += fmt.Sprintf("\n\n*%s*\n%s", descLabel, c.Description)
	}
	blocks = append(blocks, goslack.NewSectionBlock(
		goslack.NewTextBlockObject(goslack.MarkdownType, body, false, false), nil, nil))

	if fieldLines := renderSummaryFields(c, entry); len(fieldLines) > 0 {
		blocks = append(blocks, goslack.NewSectionBlock(
			goslack.NewTextBlockObject(goslack.MarkdownType, strings.Join(fieldLines, "\n"), false, false), nil, nil))
	}

	if status := renderBoardStatus(c, entry); status != "" {
		blocks = append(blocks, goslack.NewContextBlock("thread_case_summary_status",
			goslack.NewTextBlockObject(goslack.MarkdownType, fmt.Sprintf("*%s*: %s", statusLabel, status), false, false)))
	}

	if url != "" {
		blocks = append(blocks, goslack.NewContextBlock("thread_case_summary_link",
			goslack.NewTextBlockObject(goslack.MarkdownType, i18n.T(ctx, i18n.MsgThreadCaseSummaryLink, url), false, false)))
	}

	return blocks, fallback
}

// buildThreadCaseSummaryLinkBlock renders the one-line summary: the header
// followed by the case title linked to the web UI, or the bare title when no
// base URL is configured. The title is escaped so it cannot break the mrkdwn
// link syntax.
func buildThreadCaseSummaryLinkBlock(ctx context.Context, c *model.Case, header, url string) goslack.Block {
	title := escapeSlackLinkLabel(orDash(c.Title))
	var line string
	if url != "" {
		line = i18n.T(ctx, i18n.MsgThreadCaseSummaryLine, header, url, title)
	} else {
		line = i18n.T(ctx, i18n.MsgThreadCaseSummaryLineNoLink, header, title)
	}
	return goslack.NewSectionBlock(
		goslack.NewTextBlockObject(goslack.MarkdownType, line, false, false), nil, nil)
}

// renderSummaryFields turns the case's populated custom fields into
// "• Name: value" lines, resolving select / multi-select option ids to their
// display names via the workspace schema.
func renderSummaryFields(c *model.Case, entry *model.WorkspaceEntry) []string {
	if len(c.FieldValues) == 0 || entry == nil || entry.FieldSchema == nil {
		return nil
	}
	var lines []string
	for _, def := range entry.FieldSchema.Fields {
		fv, ok := c.FieldValues[def.ID]
		if !ok {
			continue
		}
		rendered := renderFieldValue(def, fv)
		if rendered == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("• *%s*: %s", def.Name, rendered))
	}
	return lines
}

func renderFieldValue(def config.FieldDefinition, fv model.FieldValue) string {
	if s, ok := semantic.SlackMrkdwn(def, fv.Value); ok {
		return s
	}
	switch def.Type {
	case types.FieldTypeSelect:
		if id, ok := fv.Value.(string); ok {
			return optionName(def, id)
		}
	case types.FieldTypeMultiSelect:
		ids := toStringSlice(fv.Value)
		if len(ids) == 0 {
			return ""
		}
		names := make([]string, 0, len(ids))
		for _, id := range ids {
			names = append(names, optionName(def, id))
		}
		return strings.Join(names, ", ")
	default:
		if fv.Value == nil {
			return ""
		}
		return fmt.Sprintf("%v", fv.Value)
	}
	return ""
}

func optionName(def config.FieldDefinition, id string) string {
	for _, o := range def.Options {
		if o.ID == id {
			return o.Name
		}
	}
	return id
}

func toStringSlice(v any) []string {
	switch s := v.(type) {
	case []string:
		return s
	case string:
		// LLMs sometimes emit a bare string for a single-option multi-select.
		if s == "" {
			return nil
		}
		return []string{s}
	case []any:
		out := make([]string, 0, len(s))
		for _, e := range s {
			if str, ok := e.(string); ok {
				out = append(out, str)
			}
		}
		return out
	default:
		return nil
	}
}

func renderBoardStatus(c *model.Case, entry *model.WorkspaceEntry) string {
	if c.BoardStatus == "" {
		return ""
	}
	if entry != nil && entry.CaseStatusSet != nil {
		if def, ok := entry.CaseStatusSet.Get(c.BoardStatus); ok {
			return def.Name
		}
	}
	return c.BoardStatus
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
