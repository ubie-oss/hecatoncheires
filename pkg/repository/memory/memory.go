package memory

import (
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
)

// Repository is an alias for Memory to match the pattern
type Repository = Memory

type Memory struct {
	caseRepo        *caseRepository
	action          *actionRepository
	memo            *memoRepository
	knowledge       *knowledgeRepository
	tag             *tagRepository
	tokens          *tokenStore
	slack           *slackRepository
	slackUser       *slackUserRepository
	source          *sourceRepository
	caseMessage     *caseMessageRepository
	actionMessage   *actionMessageRepository
	actionEvent     *actionEventRepository
	actionStep      *actionStepRepository
	actionComment   *actionCommentRepository
	assistLog       *assistLogRepository
	caseProposal    *caseProposalRepository
	session         *sessionRepository
	notifySlot      *notificationSlotRepository
	jobRun          *jobRunRepository
	jobRunLog       *jobRunLogRepository
	jobRunEvent     *jobRunEventRepository
	jobSlot         *jobSlotRepository
	importRepo      *importRepository
	reactionClaim   *reactionClaimRepository
	userPreference  *userPreferenceRepository
	homeMessage     *homeMessageRepository
	assigneeRanking *assigneeRankingRepository
	slackMemberChs  *slackMemberChannelsRepository
}

var _ interfaces.Repository = &Memory{}

func New() *Memory {
	return &Memory{
		caseRepo:        newCaseRepository(),
		action:          newActionRepository(),
		memo:            newMemoRepository(),
		knowledge:       newKnowledgeRepository(),
		tag:             newTagRepository(),
		tokens:          newTokenStore(),
		slack:           newSlackRepository(),
		slackUser:       newSlackUserRepository(),
		source:          newSourceRepository(),
		caseMessage:     newCaseMessageRepository(),
		actionMessage:   newActionMessageRepository(),
		actionEvent:     newActionEventRepository(),
		actionStep:      newActionStepRepository(),
		actionComment:   newActionCommentRepository(),
		assistLog:       newAssistLogRepository(),
		caseProposal:    newCaseProposalRepository(),
		session:         newSessionRepository(),
		notifySlot:      newNotificationSlotRepository(),
		jobRun:          newJobRunRepository(),
		jobRunLog:       newJobRunLogRepository(),
		jobRunEvent:     newJobRunEventRepository(),
		jobSlot:         newJobSlotRepository(),
		importRepo:      newImportRepository(),
		reactionClaim:   newReactionClaimRepository(),
		userPreference:  newUserPreferenceRepository(),
		homeMessage:     newHomeMessageRepository(),
		assigneeRanking: newAssigneeRankingRepository(),
		slackMemberChs:  newSlackMemberChannelsRepository(),
	}
}

func (m *Memory) Case() interfaces.CaseRepository {
	return m.caseRepo
}

func (m *Memory) Action() interfaces.ActionRepository {
	return m.action
}

func (m *Memory) Memo() interfaces.MemoRepository {
	return m.memo
}

func (m *Memory) Knowledge() interfaces.KnowledgeRepository {
	return m.knowledge
}

func (m *Memory) Tag() interfaces.TagRepository {
	return m.tag
}

func (m *Memory) Slack() interfaces.SlackRepository {
	return m.slack
}

func (m *Memory) SlackUser() interfaces.SlackUserRepository {
	return m.slackUser
}

func (m *Memory) Source() interfaces.SourceRepository {
	return m.source
}

func (m *Memory) CaseMessage() interfaces.CaseMessageRepository {
	return m.caseMessage
}

func (m *Memory) ActionMessage() interfaces.ActionMessageRepository {
	return m.actionMessage
}

func (m *Memory) ActionEvent() interfaces.ActionEventRepository {
	return m.actionEvent
}

func (m *Memory) ActionStep() interfaces.ActionStepRepository {
	return m.actionStep
}

func (m *Memory) ActionComment() interfaces.ActionCommentRepository {
	return m.actionComment
}

func (m *Memory) AssistLog() interfaces.AssistLogRepository {
	return m.assistLog
}

func (m *Memory) CaseProposal() interfaces.CaseProposalRepository {
	return m.caseProposal
}

func (m *Memory) Session() interfaces.SessionRepository {
	return m.session
}

func (m *Memory) NotificationSlot() interfaces.NotificationSlotRepository {
	return m.notifySlot
}

func (m *Memory) JobRun() interfaces.JobRunRepository {
	return m.jobRun
}

func (m *Memory) JobRunLog() interfaces.JobRunLogRepository {
	return m.jobRunLog
}

func (m *Memory) JobRunEvent() interfaces.JobRunEventRepository {
	return m.jobRunEvent
}

func (m *Memory) JobSlot() interfaces.JobSlotRepository {
	return m.jobSlot
}

func (m *Memory) Import() interfaces.ImportRepository {
	return m.importRepo
}

func (m *Memory) ReactionClaim() interfaces.ReactionClaimRepository {
	return m.reactionClaim
}

func (m *Memory) UserPreference() interfaces.UserPreferenceRepository {
	return m.userPreference
}

func (m *Memory) HomeMessage() interfaces.HomeMessageRepository {
	return m.homeMessage
}

func (m *Memory) AssigneeRanking() interfaces.AssigneeRankingRepository {
	return m.assigneeRanking
}

func (m *Memory) SlackMemberChannels() interfaces.SlackMemberChannelsRepository {
	return m.slackMemberChs
}

func (m *Memory) Close() error {
	// No resources to clean up for in-memory repository
	return nil
}
