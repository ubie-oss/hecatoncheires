package interfaces

import (
	"context"

	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/auth"
)

// Repository defines the interface for data persistence
type Repository interface {
	Case() CaseRepository
	Action() ActionRepository
	Memo() MemoRepository
	Knowledge() KnowledgeRepository
	Tag() TagRepository
	Slack() SlackRepository
	SlackUser() SlackUserRepository
	Source() SourceRepository
	CaseMessage() CaseMessageRepository
	ActionMessage() ActionMessageRepository
	ActionEvent() ActionEventRepository
	ActionStep() ActionStepRepository
	ActionComment() ActionCommentRepository
	AssistLog() AssistLogRepository
	CaseProposal() CaseProposalRepository
	Session() SessionRepository
	NotificationSlot() NotificationSlotRepository
	JobRun() JobRunRepository
	JobRunLog() JobRunLogRepository
	JobRunEvent() JobRunEventRepository
	JobSlot() JobSlotRepository
	Import() ImportRepository
	ReactionClaim() ReactionClaimRepository
	UserPreference() UserPreferenceRepository
	HomeMessage() HomeMessageRepository
	AssigneeRanking() AssigneeRankingRepository
	SlackMemberChannels() SlackMemberChannelsRepository

	// Auth methods
	PutToken(ctx context.Context, token *auth.Token) error
	GetToken(ctx context.Context, tokenID auth.TokenID) (*auth.Token, error)
	DeleteToken(ctx context.Context, tokenID auth.TokenID) error

	// Close closes the repository and releases any resources
	Close() error
}
