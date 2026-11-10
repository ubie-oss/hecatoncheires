package model_test

import (
	"testing"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
)

func TestSystemActorID(t *testing.T) {
	gt.String(t, model.SystemActorID).Equal("@system")
}

func TestIsSystemActor(t *testing.T) {
	gt.Bool(t, model.IsSystemActor(model.SystemActorID)).True()
	gt.Bool(t, model.IsSystemActor("@system")).True()
	gt.Bool(t, model.IsSystemActor("U12345")).False()
	gt.Bool(t, model.IsSystemActor("W67890")).False()
	gt.Bool(t, model.IsSystemActor("")).False()
}

func TestCaseChannelActor(t *testing.T) {
	id := model.CaseChannelActorID("ws-1", "C0CASE01")
	gt.Bool(t, model.IsCaseChannelActor(id)).True()
	ws, ch, ok := model.ParseCaseChannelActor(id)
	gt.Bool(t, ok).True()
	gt.Value(t, ws).Equal("ws-1")
	gt.Value(t, ch).Equal("C0CASE01")

	// Either half missing is not an actor.
	gt.Bool(t, model.IsCaseChannelActor(model.CaseChannelActorID("", "C0CASE01"))).False()
	gt.Bool(t, model.IsCaseChannelActor(model.CaseChannelActorID("ws-1", ""))).False()
	gt.Bool(t, model.IsCaseChannelActor("@channel:C0CASE01")).False()
	// Other actors are not case-channel actors.
	gt.Bool(t, model.IsCaseChannelActor("C0CASE01")).False()
	gt.Bool(t, model.IsCaseChannelActor("U12345")).False()
	gt.Bool(t, model.IsCaseChannelActor(model.SystemActorID)).False()
	gt.Bool(t, model.IsCaseChannelActor("")).False()
	// Distinct channels and workspaces give distinct actors.
	gt.Value(t, model.CaseChannelActorID("ws-1", "C0CASE02")).NotEqual(id)
	gt.Value(t, model.CaseChannelActorID("ws-2", "C0CASE01")).NotEqual(id)
}
