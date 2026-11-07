package slack

import (
	"context"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	slackmodel "github.com/m-mizutani/hecatoncheires/pkg/domain/model/slack"
)

// MessageFromEvent builds the domain Message for a Slack Events API callback,
// or nil when the event is not one we ingest. Decoding the Slack wire format
// belongs here rather than in the domain model: the body may live in `blocks`
// or `attachments` instead of `text`, and resolving that is protocol work.
func MessageFromEvent(_ context.Context, ev *slackevents.EventsAPIEvent) *slackmodel.Message {
	if ev == nil || ev.Type != slackevents.CallbackEvent {
		return nil
	}

	now := time.Now()

	switch evt := ev.InnerEvent.Data.(type) {
	case *slackevents.AppMentionEvent:
		return slackmodel.NewMessageFromData(
			evt.TimeStamp,
			evt.Channel,
			evt.ThreadTimeStamp,
			ev.TeamID,
			evt.User,
			evt.User, // Default to user ID, will be updated later if needed
			MessageBody(evt.Text, evt.Blocks, evt.Attachments),
			evt.EventTimeStamp,
			now,
			nil,
		).WithBotID(evt.BotID)

	case *slackevents.MessageEvent:
		threadTS := ""
		if evt.ThreadTimeStamp != "" && evt.ThreadTimeStamp != evt.TimeStamp {
			threadTS = evt.ThreadTimeStamp
		}
		// MessageEvent carries `blocks` at the top level but NOT `attachments`;
		// slack-go's custom unmarshaller re-decodes the whole payload into
		// evt.Message, which is where the attachments (and the files below)
		// land. An integration such as the GitHub Slack app puts the entire
		// notification there and leaves evt.Text empty.
		var (
			files       []slackmodel.File
			attachments []slack.Attachment
		)
		if evt.Message != nil {
			for _, f := range evt.Message.Files {
				files = append(files, FileFromSlack(f))
			}
			attachments = evt.Message.Attachments
		}
		return slackmodel.NewMessageFromData(
			evt.TimeStamp,
			evt.Channel,
			threadTS,
			ev.TeamID,
			evt.User,
			evt.User, // Default to user ID
			MessageBody(evt.Text, evt.Blocks, attachments),
			evt.EventTimeStamp,
			now,
			files,
		).WithBotID(evt.BotID)

	default:
		return nil
	}
}

// FileFromSlack converts a slack-go File into the domain File.
func FileFromSlack(f slack.File) slackmodel.File {
	return slackmodel.NewFileFromData(
		f.ID,
		f.Name,
		f.Mimetype,
		f.Filetype,
		f.Size,
		f.URLPrivate,
		f.Permalink,
		bestThumbURL(f),
	)
}

// bestThumbURL selects the best available thumbnail URL from a Slack file.
// It prefers larger thumbnails for better display quality.
func bestThumbURL(f slack.File) string {
	// Prefer larger thumbnails, fall back to smaller ones
	candidates := []string{
		f.Thumb1024,
		f.Thumb960,
		f.Thumb720,
		f.Thumb480,
		f.Thumb360,
		f.Thumb160,
		f.Thumb80,
		f.Thumb64,
	}

	for _, url := range candidates {
		if url != "" {
			return url
		}
	}
	return ""
}
