package slack_test

import (
	"testing"
	"time"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/slack"
)

func TestNewMessageFromData(t *testing.T) {
	createdAt := time.Now()

	msg := slack.NewMessageFromData(
		"1234567890.123456",
		"C123456",
		"1234567890.123455",
		"T123456",
		"U123456",
		"john_doe",
		"Test message",
		"1234567890.123456",
		createdAt,
		nil,
	)

	gt.Value(t, msg).NotNil().Required()
	gt.Value(t, msg.ID()).Equal("1234567890.123456")
	gt.Value(t, msg.ChannelID()).Equal("C123456")
	gt.Value(t, msg.ThreadTS()).Equal("1234567890.123455")
	gt.Value(t, msg.TeamID()).Equal("T123456")
	gt.Value(t, msg.UserID()).Equal("U123456")
	gt.Value(t, msg.UserName()).Equal("john_doe")
	gt.Value(t, msg.Text()).Equal("Test message")
	gt.Value(t, msg.EventTS()).Equal("1234567890.123456")
	gt.Value(t, msg.CreatedAt()).Equal(createdAt)
	gt.Array(t, msg.Files()).Length(0)
}

func TestNewMessageFromData_WithFiles(t *testing.T) {
	createdAt := time.Now()
	files := []slack.File{
		slack.NewFileFromData("F001", "test.png", "image/png", "png", 1024,
			"https://files.slack.com/private", "https://slack.com/permalink", "https://slack.com/thumb"),
	}

	msg := slack.NewMessageFromData(
		"1234567890.123456", "C123456", "", "T123456",
		"U123456", "user", "test", "1234567890.123456",
		createdAt, files,
	)

	gt.Value(t, msg).NotNil().Required()
	gt.Array(t, msg.Files()).Length(1).Required()
	gt.Value(t, msg.Files()[0].ID()).Equal("F001")
	gt.Value(t, msg.Files()[0].Name()).Equal("test.png")
}

func TestMessage_WithBotID(t *testing.T) {
	createdAt := time.Now()
	files := []slack.File{
		slack.NewFileFromData("F001", "test.png", "image/png", "png", 1024,
			"https://files.example.com/private", "https://example.com/permalink", "https://example.com/thumb"),
	}
	orig := slack.NewMessageFromData(
		"1234567890.123456", "C123456", "", "T123456",
		"", "", "posted by an app", "1234567890.123456",
		createdAt, files,
	)
	gt.Value(t, orig.BotID()).Equal("")

	withBot := orig.WithBotID("B0WORKFLOW1")
	gt.Value(t, withBot.BotID()).Equal("B0WORKFLOW1")
	// Every other field is carried over unchanged.
	gt.Value(t, withBot.ID()).Equal("1234567890.123456")
	gt.Value(t, withBot.ChannelID()).Equal("C123456")
	gt.Value(t, withBot.TeamID()).Equal("T123456")
	gt.Value(t, withBot.UserID()).Equal("")
	gt.Value(t, withBot.Text()).Equal("posted by an app")
	gt.Value(t, withBot.EventTS()).Equal("1234567890.123456")
	gt.Value(t, withBot.CreatedAt()).Equal(createdAt)
	gt.Array(t, withBot.Files()).Length(1).Required()
	gt.Value(t, withBot.Files()[0].ID()).Equal("F001")
	// The receiver is left unchanged.
	gt.Value(t, orig.BotID()).Equal("")
}
