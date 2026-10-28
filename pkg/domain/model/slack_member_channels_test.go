package model_test

import (
	"testing"
	"time"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
)

func TestSlackMemberChannels_IsFresh(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	window := 10 * time.Minute

	var nilList *model.SlackMemberChannels
	gt.Bool(t, nilList.IsFresh(now, window)).False()
	gt.Bool(t, (&model.SlackMemberChannels{}).IsFresh(now, window)).False()
	gt.Bool(t, (&model.SlackMemberChannels{FetchedAt: now.Add(-window + time.Second)}).IsFresh(now, window)).True()
	gt.Bool(t, (&model.SlackMemberChannels{FetchedAt: now.Add(-window)}).IsFresh(now, window)).False()
}

func TestSlackMemberChannels_Validate(t *testing.T) {
	valid := func() *model.SlackMemberChannels {
		return &model.SlackMemberChannels{
			WorkspaceID: "ws",
			UserID:      "U1",
			Channels:    []model.SlackMemberChannel{{ID: "C1", Name: "general"}},
			FetchedAt:   time.Now(),
		}
	}
	gt.NoError(t, valid().Validate())

	empty := valid()
	empty.Channels = nil
	gt.NoError(t, empty.Validate())

	var nilList *model.SlackMemberChannels
	gt.Error(t, nilList.Validate()).Is(model.ErrSlackMemberChannelsValidation)

	for name, mutate := range map[string]func(*model.SlackMemberChannels){
		"no workspace":     func(m *model.SlackMemberChannels) { m.WorkspaceID = "" },
		"no user":          func(m *model.SlackMemberChannels) { m.UserID = "" },
		"no fetched at":    func(m *model.SlackMemberChannels) { m.FetchedAt = time.Time{} },
		"empty channel id": func(m *model.SlackMemberChannels) { m.Channels[0].ID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			m := valid()
			mutate(m)
			gt.Error(t, m.Validate()).Is(model.ErrSlackMemberChannelsValidation)
		})
	}
}

func TestSlackMemberChannelsRefresh_Validate(t *testing.T) {
	gt.NoError(t, (&model.SlackMemberChannelsRefresh{WorkspaceID: "ws", UserID: "U1", ClaimedAt: time.Now()}).Validate())

	var nilClaim *model.SlackMemberChannelsRefresh
	gt.Error(t, nilClaim.Validate()).Is(model.ErrSlackMemberChannelsValidation)
	gt.Error(t, (&model.SlackMemberChannelsRefresh{UserID: "U1", ClaimedAt: time.Now()}).Validate()).Is(model.ErrSlackMemberChannelsValidation)
	gt.Error(t, (&model.SlackMemberChannelsRefresh{WorkspaceID: "ws", ClaimedAt: time.Now()}).Validate()).Is(model.ErrSlackMemberChannelsValidation)
	gt.Error(t, (&model.SlackMemberChannelsRefresh{WorkspaceID: "ws", UserID: "U1"}).Validate()).Is(model.ErrSlackMemberChannelsValidation)
}
