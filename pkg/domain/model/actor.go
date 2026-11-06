package model

// SystemActorID is the sentinel ActorUserID used when a Job's tool invokes
// a mutation on behalf of the system (no human user). The '@' prefix ensures
// the value never collides with Slack user IDs (which always start with 'U'
// or 'W').
const SystemActorID = "@system"

// IsSystemActor reports whether the given actor user ID is the system
// sentinel. Permission checks that key off a Slack user identity must skip
// for system actor — there is no human to authenticate as.
func IsSystemActor(actorUserID string) bool {
	return actorUserID == SystemActorID
}

// caseChannelActorPrefix marks an actor that is a case channel rather than a
// person. Like SystemActorID it starts with '@', so it can never collide with
// a Slack user ID.
const caseChannelActorPrefix = "@channel:"

// CaseChannelActorID returns the actor that stands for the case channel
// channelID itself.
//
// It is the access actor of an agent run that an app (a Slack workflow, an
// integration bot) started by mentioning the bot in a channel-mode case
// channel. Such a mention names no Slack user, and running with no actor at
// all would be read as a system context that bypasses private-case access
// control. This actor is deliberately narrow instead: it may access the case
// bound to that channel (whatever the run writes back lands in that channel,
// in front of the members who can already see the case) and public cases, and
// no other private case — it is never one of a case's ChannelUserIDs.
func CaseChannelActorID(channelID string) string {
	return caseChannelActorPrefix + channelID
}

// IsCaseChannelActor reports whether actorID is an actor built by
// CaseChannelActorID.
func IsCaseChannelActor(actorID string) bool {
	return len(actorID) > len(caseChannelActorPrefix) &&
		actorID[:len(caseChannelActorPrefix)] == caseChannelActorPrefix
}
