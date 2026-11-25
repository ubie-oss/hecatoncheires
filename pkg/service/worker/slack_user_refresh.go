package worker

import (
	"context"
	"time"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/errutil"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/logging"
)

// SlackUserRefreshWorker manages background refresh of Slack users from Slack API to database

type SlackUserRefreshWorker struct {
	repo         interfaces.Repository
	slackService slack.Service
	interval     time.Duration
	isOrgLevel   bool
	stopCh       chan struct{}
	doneCh       chan struct{}
}

// NewSlackUserRefreshWorker creates a new worker for refreshing Slack users.
// If isOrgLevel is true, the worker will call ListTeams to discover all workspaces
// and then ListUsers per workspace.
func NewSlackUserRefreshWorker(repo interfaces.Repository, slackSvc slack.Service, interval time.Duration, isOrgLevel bool) *SlackUserRefreshWorker {
	return &SlackUserRefreshWorker{
		repo:         repo,
		slackService: slackSvc,
		interval:     interval,
		isOrgLevel:   isOrgLevel,
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
	}
}

// Start begins the background refresh loop
// - Initial sync and periodic refresh both run in a background goroutine
// - Does not block server startup
func (w *SlackUserRefreshWorker) Start(ctx context.Context) error {
	logging.Default().Info("Slack user refresh worker starting",
		"interval", w.interval.String())

	go w.run(ctx)

	return nil
}

// Stop signals the worker to stop and waits for completion
func (w *SlackUserRefreshWorker) Stop() {
	logging.Default().Info("Slack user refresh worker stopping")
	close(w.stopCh)
	<-w.doneCh
	logging.Default().Info("Slack user refresh worker stopped")
}

// run is the main worker loop (runs in goroutine)
func (w *SlackUserRefreshWorker) run(ctx context.Context) {
	defer close(w.doneCh)

	// Initial sync (runs in goroutine, does not block server startup)
	if err := w.refresh(ctx); err != nil {
		errutil.Handle(ctx, goerr.Wrap(err, "Initial Slack user refresh failed (will retry next interval)"), "Initial Slack user refresh failed")
	}

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := w.refresh(ctx); err != nil {
				// Continue worker; report so the operator can see repeated failures.
				errutil.Handle(ctx, goerr.Wrap(err, "Slack user refresh failed (will retry next interval)"), "Slack user refresh failed")
			}

		case <-w.stopCh:
			logging.Default().Info("Slack user refresh worker received stop signal")
			return

		case <-ctx.Done():
			logging.Default().Info("Slack user refresh worker context cancelled")
			return
		}
	}
}

// refresh fetches all members before publishing the replacement directory.
func (w *SlackUserRefreshWorker) refresh(ctx context.Context) error {
	startTime := time.Now()
	logging.Default().Info("Starting Slack user refresh")

	if err := w.repo.SlackUser().RecordRefreshAttempt(ctx, startTime); err != nil {
		return goerr.Wrap(err, "failed to record Slack user refresh")
	}

	// Fetch all users from Slack API
	var slackUsers []*slack.User
	if w.isOrgLevel {
		// Org-level app: discover all workspaces and fetch users per workspace
		teams, err := w.slackService.ListTeams(ctx)
		if err != nil {
			return goerr.Wrap(err, "failed to list teams for org-level user refresh")
		}

		seen := make(map[string]struct{})
		for _, team := range teams {
			users, err := w.slackService.ListUsers(ctx, team.ID)
			if err != nil {
				return goerr.Wrap(err, "failed to list Slack users from API",
					goerr.V("team_id", team.ID),
					goerr.V("team_name", team.Name))
			}
			for _, u := range users {
				if _, ok := seen[u.ID]; !ok {
					seen[u.ID] = struct{}{}
					slackUsers = append(slackUsers, u)
				}
			}
		}

		logging.Default().Info("fetched users from all workspaces",
			"team_count", len(teams),
			"unique_user_count", len(slackUsers),
		)
	} else {
		// WS-level app: single call without team_id
		var err error
		slackUsers, err = w.slackService.ListUsers(ctx, "")
		if err != nil {
			return goerr.Wrap(err, "failed to list Slack users from API")
		}
	}

	// Convert to domain models
	users := make([]*model.SlackUser, len(slackUsers))
	for i, su := range slackUsers {
		users[i] = &model.SlackUser{
			ID:        model.SlackUserID(su.ID),
			Name:      su.Name,
			RealName:  su.RealName,
			Email:     su.Email,
			ImageURL:  su.ImageURL,
			UpdatedAt: startTime,
		}
	}

	if err := w.repo.SlackUser().ReplaceAll(ctx, users, startTime); err != nil {
		return goerr.Wrap(err, "failed to publish Slack user directory", goerr.V("count", len(users)))
	}

	duration := time.Since(startTime)
	logging.Default().Info("Slack user refresh completed",
		"count", len(users),
		"duration", duration.String())

	return nil
}
