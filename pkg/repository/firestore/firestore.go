package firestore

import (
	"context"

	"cloud.google.com/go/firestore"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
)

type Firestore struct {
	client          *firestore.Client
	caseRepo        *caseRepository
	action          *actionRepository
	memo            *memoRepository
	knowledge       *knowledgeRepository
	tag             *tagRepository
	slack           *slackRepository
	slackUser       *slackUserRepository
	source          *sourceRepository
	caseMessage     *caseMessageRepository
	actionMessage   *actionMessageRepository
	actionEvent     *actionEventRepository
	actionStep      *actionStepRepository
	actionComment   *actionCommentRepository
	assistLog       *firestoreAssistLogRepository
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

var _ interfaces.Repository = &Firestore{}

func New(ctx context.Context, projectID, databaseID string) (*Firestore, error) {
	var client *firestore.Client
	var err error
	if databaseID != "" {
		client, err = firestore.NewClientWithDatabase(ctx, projectID, databaseID)
	} else {
		client, err = firestore.NewClient(ctx, projectID)
	}
	if err != nil {
		return nil, goerr.Wrap(err, "failed to create firestore client",
			goerr.V("projectID", projectID),
			goerr.V("databaseID", databaseID),
		)
	}

	f := &Firestore{
		client:          client,
		caseRepo:        newCaseRepository(client),
		action:          newActionRepository(client),
		memo:            newMemoRepository(client),
		knowledge:       newKnowledgeRepository(client),
		tag:             newTagRepository(client),
		slack:           newSlackRepository(client),
		slackUser:       newSlackUserRepository(client),
		source:          newSourceRepository(client),
		caseMessage:     newCaseMessageRepository(client),
		actionMessage:   newActionMessageRepository(client),
		actionEvent:     newActionEventRepository(client),
		actionStep:      newActionStepRepository(client),
		actionComment:   newActionCommentRepository(client),
		assistLog:       newFirestoreAssistLogRepository(client),
		caseProposal:    newCaseProposalRepository(client),
		session:         newSessionRepository(client),
		notifySlot:      newNotificationSlotRepository(client),
		jobRun:          newJobRunRepository(client),
		jobRunLog:       newJobRunLogRepository(client),
		jobRunEvent:     newJobRunEventRepository(client),
		jobSlot:         newJobSlotRepository(client),
		importRepo:      newImportRepository(client),
		reactionClaim:   newReactionClaimRepository(client),
		userPreference:  newUserPreferenceRepository(client),
		homeMessage:     newHomeMessageRepository(client),
		assigneeRanking: newAssigneeRankingRepository(client),
		slackMemberChs:  newSlackMemberChannelsRepository(client),
	}

	return f, nil
}

func (f *Firestore) Case() interfaces.CaseRepository {
	return f.caseRepo
}

func (f *Firestore) Action() interfaces.ActionRepository {
	return f.action
}

func (f *Firestore) Memo() interfaces.MemoRepository {
	return f.memo
}

func (f *Firestore) Knowledge() interfaces.KnowledgeRepository {
	return f.knowledge
}

func (f *Firestore) Tag() interfaces.TagRepository {
	return f.tag
}

func (f *Firestore) Slack() interfaces.SlackRepository {
	return f.slack
}

func (f *Firestore) SlackUser() interfaces.SlackUserRepository {
	return f.slackUser
}

func (f *Firestore) Source() interfaces.SourceRepository {
	return f.source
}

func (f *Firestore) CaseMessage() interfaces.CaseMessageRepository {
	return f.caseMessage
}

func (f *Firestore) ActionMessage() interfaces.ActionMessageRepository {
	return f.actionMessage
}

func (f *Firestore) ActionEvent() interfaces.ActionEventRepository {
	return f.actionEvent
}

func (f *Firestore) ActionStep() interfaces.ActionStepRepository {
	return f.actionStep
}

func (f *Firestore) ActionComment() interfaces.ActionCommentRepository {
	return f.actionComment
}

func (f *Firestore) AssistLog() interfaces.AssistLogRepository {
	return f.assistLog
}

func (f *Firestore) CaseProposal() interfaces.CaseProposalRepository {
	return f.caseProposal
}

func (f *Firestore) Session() interfaces.SessionRepository {
	return f.session
}

func (f *Firestore) NotificationSlot() interfaces.NotificationSlotRepository {
	return f.notifySlot
}

func (f *Firestore) JobRun() interfaces.JobRunRepository {
	return f.jobRun
}

func (f *Firestore) JobRunLog() interfaces.JobRunLogRepository {
	return f.jobRunLog
}

func (f *Firestore) JobRunEvent() interfaces.JobRunEventRepository {
	return f.jobRunEvent
}

func (f *Firestore) JobSlot() interfaces.JobSlotRepository {
	return f.jobSlot
}

func (f *Firestore) Import() interfaces.ImportRepository {
	return f.importRepo
}

func (f *Firestore) ReactionClaim() interfaces.ReactionClaimRepository {
	return f.reactionClaim
}

func (f *Firestore) UserPreference() interfaces.UserPreferenceRepository {
	return f.userPreference
}

func (f *Firestore) HomeMessage() interfaces.HomeMessageRepository {
	return f.homeMessage
}

func (f *Firestore) AssigneeRanking() interfaces.AssigneeRankingRepository {
	return f.assigneeRanking
}

func (f *Firestore) SlackMemberChannels() interfaces.SlackMemberChannelsRepository {
	return f.slackMemberChs
}

func (f *Firestore) Close() error {
	if f.client != nil {
		return f.client.Close()
	}
	return nil
}
