package model

import (
	"strings"
	"time"

	"github.com/m-mizutani/goerr/v2"
)

// ErrSlackUserValidation is returned when a SlackUser fails its persistence-boundary invariants.
var ErrSlackUserValidation = goerr.New("slack user validation failed")

// SlackUserID represents a unique identifier for a Slack user
type SlackUserID string

// SlackUser represents a Slack workspace user stored in the database
type SlackUser struct {
	ID              SlackUserID
	Name            string    // Slack username / handle (e.g., "john.doe")
	RealName        string    // User-facing display name (Profile.DisplayName, with Profile.RealName / RealName as fallback). Field is named "RealName" for legacy reasons.
	Email           string    // Email address as reported by Slack
	NormalizedEmail string    // Indexed lookup value, derived by the repository on write
	ImageURL        string    // Avatar URL (empty string = no image)
	UpdatedAt       time.Time // Last synchronized from Slack
}

// NormalizeSlackEmail applies the same case folding to token and directory emails.
func NormalizeSlackEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// Validate enforces the invariants required before any persistence write.
// ID is the natural key (the Slack user id), so a user record with no ID
// fails loudly here instead of landing in storage under an empty key.
func (u *SlackUser) Validate() error {
	if u == nil {
		return goerr.Wrap(ErrSlackUserValidation, "slack user is nil")
	}
	if u.ID == "" {
		return goerr.Wrap(ErrSlackUserValidation, "slack user ID is required")
	}
	return nil
}

// SlackUserMetadata tracks the health and status of Slack user synchronization
type SlackUserMetadata struct {
	LastRefreshSuccess time.Time // Last successful refresh time
	LastRefreshAttempt time.Time // Last refresh attempt time (success or failure)
	UserCount          int       // Number of users at last successful refresh
}
