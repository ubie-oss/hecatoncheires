package model_test

import (
	"errors"
	"testing"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/config"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
)

func TestNewWorkspaceRegistry(t *testing.T) {
	reg := model.NewWorkspaceRegistry()
	gt.Value(t, reg).NotNil()
	gt.Array(t, reg.List()).Length(0)
	gt.Array(t, reg.Workspaces()).Length(0)
}

func TestWorkspaceRegistry_Register(t *testing.T) {
	reg := model.NewWorkspaceRegistry()

	entry := &model.WorkspaceEntry{
		Workspace: model.Workspace{
			ID:   "risk",
			Name: "Risk Management",
		},
		FieldSchema: &config.FieldSchema{
			Labels: config.EntityLabels{Case: "Risk"},
		},
	}
	reg.Register(entry)

	gt.Array(t, reg.List()).Length(1)
	gt.Array(t, reg.Workspaces()).Length(1)
	gt.Value(t, reg.Workspaces()[0].ID).Equal("risk")
	gt.Value(t, reg.Workspaces()[0].Name).Equal("Risk Management")
}

func TestWorkspaceRegistry_RegisterMultiple(t *testing.T) {
	reg := model.NewWorkspaceRegistry()

	reg.Register(&model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: "risk", Name: "Risk Management"},
		FieldSchema: &config.FieldSchema{},
	})
	reg.Register(&model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: "recruit", Name: "Recruitment"},
		FieldSchema: &config.FieldSchema{},
	})

	gt.Array(t, reg.List()).Length(2)
	gt.Array(t, reg.Workspaces()).Length(2)

	// Verify registration order is preserved
	workspaces := reg.Workspaces()
	gt.Value(t, workspaces[0].ID).Equal("risk")
	gt.Value(t, workspaces[1].ID).Equal("recruit")
}

func TestWorkspaceRegistry_RegisterOverwrite(t *testing.T) {
	reg := model.NewWorkspaceRegistry()

	reg.Register(&model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: "risk", Name: "Old Name"},
		FieldSchema: &config.FieldSchema{},
	})
	reg.Register(&model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: "risk", Name: "New Name"},
		FieldSchema: &config.FieldSchema{},
	})

	// Should not duplicate the entry
	gt.Array(t, reg.List()).Length(1)
	gt.Array(t, reg.Workspaces()).Length(1)

	// Should have the updated name
	gt.Value(t, reg.Workspaces()[0].Name).Equal("New Name")
}

func TestWorkspaceRegistry_Get(t *testing.T) {
	reg := model.NewWorkspaceRegistry()

	schema := &config.FieldSchema{
		Labels: config.EntityLabels{Case: "Risk"},
		Fields: []config.FieldDefinition{
			{ID: "category", Name: "Category", Type: "text"},
		},
	}
	reg.Register(&model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: "risk", Name: "Risk Management"},
		FieldSchema: schema,
	})

	entry, err := reg.Get("risk")
	gt.NoError(t, err)
	gt.Value(t, entry).NotNil()
	gt.Value(t, entry.Workspace.ID).Equal("risk")
	gt.Value(t, entry.Workspace.Name).Equal("Risk Management")
	gt.Value(t, entry.FieldSchema.Labels.Case).Equal("Risk")
	gt.Array(t, entry.FieldSchema.Fields).Length(1)
	gt.Value(t, entry.FieldSchema.Fields[0].ID).Equal("category")
}

func TestWorkspaceRegistry_GetNotFound(t *testing.T) {
	reg := model.NewWorkspaceRegistry()

	entry, err := reg.Get("nonexistent")
	gt.Value(t, entry).Nil()
	gt.Value(t, err).NotNil()
	gt.Bool(t, errors.Is(err, model.ErrWorkspaceNotFound)).True()
}

func TestWorkspaceRegistry_List(t *testing.T) {
	reg := model.NewWorkspaceRegistry()

	reg.Register(&model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: "alpha", Name: "Alpha"},
		FieldSchema: &config.FieldSchema{},
	})
	reg.Register(&model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: "beta", Name: "Beta"},
		FieldSchema: &config.FieldSchema{},
	})
	reg.Register(&model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: "gamma", Name: "Gamma"},
		FieldSchema: &config.FieldSchema{},
	})

	entries := reg.List()
	gt.Array(t, entries).Length(3)

	// Verify registration order is preserved
	gt.Value(t, entries[0].Workspace.ID).Equal("alpha")
	gt.Value(t, entries[1].Workspace.ID).Equal("beta")
	gt.Value(t, entries[2].Workspace.ID).Equal("gamma")

	// Verify each entry has its FieldSchema
	for _, e := range entries {
		gt.Value(t, e.FieldSchema).NotNil()
	}
}

func TestWorkspaceRegistry_Workspaces(t *testing.T) {
	reg := model.NewWorkspaceRegistry()

	reg.Register(&model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: "risk", Name: "Risk Management"},
		FieldSchema: &config.FieldSchema{},
	})
	reg.Register(&model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: "recruit", Name: "Recruitment"},
		FieldSchema: &config.FieldSchema{},
	})

	workspaces := reg.Workspaces()
	gt.Array(t, workspaces).Length(2)
	gt.Value(t, workspaces[0].ID).Equal("risk")
	gt.Value(t, workspaces[0].Name).Equal("Risk Management")
	gt.Value(t, workspaces[1].ID).Equal("recruit")
	gt.Value(t, workspaces[1].Name).Equal("Recruitment")
}

func TestWorkspaceEntry_IsThreadMode(t *testing.T) {
	gt.Bool(t, (&model.WorkspaceEntry{CaseMode: model.CaseModeThread}).IsThreadMode()).True()
	gt.Bool(t, (&model.WorkspaceEntry{CaseMode: model.CaseModeChannel}).IsThreadMode()).False()
	// Empty mode normalises to channel.
	gt.Bool(t, (&model.WorkspaceEntry{}).IsThreadMode()).False()

	var nilEntry *model.WorkspaceEntry
	gt.Bool(t, nilEntry.IsThreadMode()).False()
}

func TestWorkspaceRegistry_Language(t *testing.T) {
	reg := model.NewWorkspaceRegistry()
	reg.Register(&model.WorkspaceEntry{Workspace: model.Workspace{ID: "ja-ws"}, Language: i18n.LangJA})
	reg.Register(&model.WorkspaceEntry{Workspace: model.Workspace{ID: "unset-ws"}})

	gt.Value(t, reg.Language("ja-ws")).Equal(i18n.LangJA)
	gt.Value(t, reg.Language("unset-ws")).Equal(i18n.Lang(""))
	gt.Value(t, reg.Language("missing-ws")).Equal(i18n.Lang(""))

	var nilReg *model.WorkspaceRegistry
	gt.Value(t, nilReg.Language("ja-ws")).Equal(i18n.Lang(""))
}

func TestWorkspaceRegistry_FindByMonitorChannel(t *testing.T) {
	reg := model.NewWorkspaceRegistry()
	reg.Register(&model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "channel-ws"},
		CaseMode:  model.CaseModeChannel,
	})
	reg.Register(&model.WorkspaceEntry{
		Workspace:             model.Workspace{ID: "thread-ws"},
		CaseMode:              model.CaseModeThread,
		SlackMonitorChannelID: "C0MONITOR",
	})

	t.Run("matches the monitored channel of a thread-mode workspace", func(t *testing.T) {
		entry, ok := reg.FindByMonitorChannel("C0MONITOR")
		gt.Bool(t, ok).True()
		gt.Value(t, entry.Workspace.ID).Equal("thread-ws")
	})

	t.Run("does not match an unknown channel", func(t *testing.T) {
		_, ok := reg.FindByMonitorChannel("C0UNKNOWN")
		gt.Bool(t, ok).False()
	})

	t.Run("empty channel never matches", func(t *testing.T) {
		_, ok := reg.FindByMonitorChannel("")
		gt.Bool(t, ok).False()
	})

	t.Run("nil registry is safe", func(t *testing.T) {
		var nilReg *model.WorkspaceRegistry
		_, ok := nilReg.FindByMonitorChannel("C0MONITOR")
		gt.Bool(t, ok).False()
	})
}

func TestWorkspaceRegistry_IsConfiguredChannel(t *testing.T) {
	reg := model.NewWorkspaceRegistry()
	reg.Register(&model.WorkspaceEntry{
		Workspace:               model.Workspace{ID: "channel-ws"},
		CaseMode:                model.CaseModeChannel,
		SlackWorkspaceChannelID: "C0WORKSPACE",
	})
	reg.Register(&model.WorkspaceEntry{
		Workspace:             model.Workspace{ID: "thread-ws"},
		CaseMode:              model.CaseModeThread,
		SlackMonitorChannelID: "C0MONITOR",
	})

	t.Run("thread-mode monitor channel is configured", func(t *testing.T) {
		gt.Bool(t, reg.IsConfiguredChannel("C0MONITOR")).True()
	})

	t.Run("channel-mode workspace channel is configured", func(t *testing.T) {
		gt.Bool(t, reg.IsConfiguredChannel("C0WORKSPACE")).True()
	})

	t.Run("unused channel is not configured", func(t *testing.T) {
		gt.Bool(t, reg.IsConfiguredChannel("C0OTHER")).False()
	})

	t.Run("empty channel is not configured", func(t *testing.T) {
		gt.Bool(t, reg.IsConfiguredChannel("")).False()
	})

	t.Run("nil registry is safe", func(t *testing.T) {
		var nilReg *model.WorkspaceRegistry
		gt.Bool(t, nilReg.IsConfiguredChannel("C0MONITOR")).False()
	})
}

func TestWorkspaceRegistry_FindByReactionEmoji(t *testing.T) {
	reg := model.NewWorkspaceRegistry()
	reg.Register(&model.WorkspaceEntry{
		Workspace:     model.Workspace{ID: "channel-ws"},
		CaseMode:      model.CaseModeChannel,
		ReactionEmoji: "ignored", // channel mode never matches
	})
	reg.Register(&model.WorkspaceEntry{
		Workspace:             model.Workspace{ID: "thread-ws"},
		CaseMode:              model.CaseModeThread,
		SlackMonitorChannelID: "C0MONITOR",
		ReactionEmoji:         "incident",
	})

	t.Run("matches the reaction emoji of a thread-mode workspace", func(t *testing.T) {
		entry, ok := reg.FindByReactionEmoji("incident")
		gt.Bool(t, ok).True()
		gt.Value(t, entry.Workspace.ID).Equal("thread-ws")
	})

	t.Run("does not match a channel-mode workspace even with the emoji set", func(t *testing.T) {
		_, ok := reg.FindByReactionEmoji("ignored")
		gt.Bool(t, ok).False()
	})

	t.Run("does not match an unknown emoji", func(t *testing.T) {
		_, ok := reg.FindByReactionEmoji("nope")
		gt.Bool(t, ok).False()
	})

	t.Run("empty emoji never matches", func(t *testing.T) {
		_, ok := reg.FindByReactionEmoji("")
		gt.Bool(t, ok).False()
	})

	t.Run("nil registry is safe", func(t *testing.T) {
		var nilReg *model.WorkspaceRegistry
		_, ok := nilReg.FindByReactionEmoji("incident")
		gt.Bool(t, ok).False()
	})
}

func TestWorkspaceRegistry_FindByWorkspaceChannel(t *testing.T) {
	reg := model.NewWorkspaceRegistry()
	reg.Register(&model.WorkspaceEntry{
		Workspace:               model.Workspace{ID: "channel-ws"},
		CaseMode:                model.CaseModeChannel,
		SlackWorkspaceChannelID: "C0WORKSPACE",
	})
	reg.Register(&model.WorkspaceEntry{
		Workspace:               model.Workspace{ID: "thread-ws"},
		CaseMode:                model.CaseModeThread,
		SlackMonitorChannelID:   "C0MONITOR",
		SlackWorkspaceChannelID: "C0WORKSPACE", // ignored: thread mode never matches
	})

	t.Run("matches the workspace channel of a channel-mode workspace", func(t *testing.T) {
		entry, ok := reg.FindByWorkspaceChannel("C0WORKSPACE")
		gt.Bool(t, ok).True()
		gt.Value(t, entry.Workspace.ID).Equal("channel-ws")
	})

	t.Run("does not match a thread-mode workspace even with the same channel ID", func(t *testing.T) {
		reg2 := model.NewWorkspaceRegistry()
		reg2.Register(&model.WorkspaceEntry{
			Workspace:               model.Workspace{ID: "thread-only"},
			CaseMode:                model.CaseModeThread,
			SlackWorkspaceChannelID: "C0ONLYTHREAD",
		})
		_, ok := reg2.FindByWorkspaceChannel("C0ONLYTHREAD")
		gt.Bool(t, ok).False()
	})

	t.Run("does not match an unknown channel", func(t *testing.T) {
		_, ok := reg.FindByWorkspaceChannel("C0UNKNOWN")
		gt.Bool(t, ok).False()
	})

	t.Run("empty channel never matches", func(t *testing.T) {
		_, ok := reg.FindByWorkspaceChannel("")
		gt.Bool(t, ok).False()
	})

	t.Run("nil registry is safe", func(t *testing.T) {
		var nilReg *model.WorkspaceRegistry
		_, ok := nilReg.FindByWorkspaceChannel("C0WORKSPACE")
		gt.Bool(t, ok).False()
	})
}

func TestWorkspaceEntry_SlackTeamID(t *testing.T) {
	reg := model.NewWorkspaceRegistry()

	reg.Register(&model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: "ws1", Name: "Workspace 1"},
		SlackTeamID: "T111",
	})
	reg.Register(&model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: "ws2", Name: "Workspace 2"},
		SlackTeamID: "",
	})

	entry1, err := reg.Get("ws1")
	gt.NoError(t, err).Required()
	gt.Value(t, entry1.SlackTeamID).Equal("T111")

	entry2, err := reg.Get("ws2")
	gt.NoError(t, err).Required()
	gt.Value(t, entry2.SlackTeamID).Equal("")
}
