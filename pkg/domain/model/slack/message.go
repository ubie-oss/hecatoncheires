package slack

import (
	"time"
)

// Message represents a Slack message domain model.
//
// Construction from a Slack Events API payload lives in pkg/service/slack
// (MessageFromEvent): decoding the Slack wire format is protocol work, and the
// domain layer holds only the resulting values.
type Message struct {
	id        string
	channelID string
	threadTS  string
	teamID    string
	userID    string
	userName  string
	botID     string
	text      string
	eventTS   string
	files     []File
	createdAt time.Time
}

// Getters to maintain immutability
func (m *Message) ID() string {
	return m.id
}

func (m *Message) ChannelID() string {
	return m.channelID
}

func (m *Message) ThreadTS() string {
	return m.threadTS
}

func (m *Message) TeamID() string {
	return m.teamID
}

func (m *Message) UserID() string {
	return m.userID
}

// BotID is the Slack bot id of the app that posted the message (a Slack
// workflow, an integration bot), empty for a post by a person.
func (m *Message) BotID() string {
	return m.botID
}

// WithBotID returns a copy of the message attributed to the app with Slack bot
// id botID. The receiver is left unchanged.
func (m *Message) WithBotID(botID string) *Message {
	c := *m
	c.files = append([]File(nil), m.files...)
	c.botID = botID
	return &c
}

func (m *Message) UserName() string {
	return m.userName
}

func (m *Message) Text() string {
	return m.text
}

func (m *Message) EventTS() string {
	return m.eventTS
}

func (m *Message) Files() []File {
	return m.files
}

func (m *Message) CreatedAt() time.Time {
	return m.createdAt
}

// NewMessageFromData creates a Message from raw data (for repository reconstruction)
func NewMessageFromData(id, channelID, threadTS, teamID, userID, userName, text, eventTS string, createdAt time.Time, files []File) *Message {
	return &Message{
		id:        id,
		channelID: channelID,
		threadTS:  threadTS,
		teamID:    teamID,
		userID:    userID,
		userName:  userName,
		text:      text,
		eventTS:   eventTS,
		files:     files,
		createdAt: createdAt,
	}
}
