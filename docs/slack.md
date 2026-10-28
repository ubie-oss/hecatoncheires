# Slack Integration

Hecatoncheires integrates with Slack for both authentication and event webhooks. This document covers the complete Slack setup process.

## Table of Contents

1. [Slack OAuth & Authentication](#slack-oauth--authentication)
2. [Slack Events API (Webhooks)](#slack-events-api-webhooks)
3. [Slack Interactivity (Action Notifications)](#slack-interactivity-action-notifications)
4. [Slack Slash Commands (Case Creation & Editing)](#slack-slash-commands-case-creation--editing)
5. [Automatic Risk Channel Creation](#automatic-risk-channel-creation)
6. [Enterprise Grid (Org-Level App) Setup](#enterprise-grid-org-level-app-setup)
7. [Message Storage and Retrieval](#message-storage-and-retrieval)
8. [Security Considerations](#security-considerations)
9. [Permissions Reference](#permissions-reference)
10. [API Endpoints](#api-endpoints)
11. [Environment Variables Reference](#environment-variables-reference)
12. [Troubleshooting](#troubleshooting)
13. [See Also](#see-also)

---

## Slack OAuth & Authentication

Slack OAuth is used for user authentication via OpenID Connect (OIDC). The system can operate in two modes:

1. **Slack OAuth Mode**: Production authentication using Slack workspace
2. **No-Auth Mode**: Development mode that skips OAuth flow but still requires a valid Slack user ID

### Quick Start

#### Basic Setup

```bash
# Required for Slack authentication
export HECATONCHEIRES_BASE_URL="https://your-domain.com"
export HECATONCHEIRES_SLACK_CLIENT_ID="your-client-id"
export HECATONCHEIRES_SLACK_CLIENT_SECRET="your-client-secret"

# Optional: for displaying user avatars
export HECATONCHEIRES_SLACK_BOT_TOKEN="xoxb-your-bot-token"
```

Then start the server:

```bash
./hecatoncheires serve
```

### Authentication Setup

#### 1. Create a Slack App

1. Go to [https://api.slack.com/apps](https://api.slack.com/apps)
2. Click "Create New App" → "From scratch"
3. Enter your app name and select your workspace
4. Click "Create App"

#### 2. Configure OAuth & Permissions

1. In your app settings, go to **OAuth & Permissions**
2. Under **Redirect URLs**, add your callback URL:

   For local development, use a tunneling service like ngrok:
   ```
   https://your-ngrok-id.ngrok.io/api/auth/callback
   ```

   For production, use your actual domain:
   ```
   https://your-domain.com/api/auth/callback
   ```

   **Note**: Slack requires HTTPS for OAuth callbacks. HTTP URLs (including `http://localhost`) are not supported.

3. Under **Scopes**:
   - **User Token Scopes** (required for user authentication):
     - `openid` (required for OpenID Connect)
     - `profile` (to get user's name and basic info)
     - `email` (to get user's email address)
     - `search:read` (optional — required only when the AI agent's `slack__search_messages` tool is enabled. The Slack `search.messages` API is User-token-only; bot tokens cannot call it. Configure `HECATONCHEIRES_SLACK_USER_OAUTH_TOKEN` and re-install the app after adding this scope.)
     - `channels:history` (optional — recommended when the AI agent's `slack__get_messages` tool is enabled. When the User OAuth Token has this scope, `conversations.replies` / `conversations.history` are called with the User token, which lets the agent read public channels the bot has not been invited to. Without it, the tool falls back to the Bot token and returns `not_in_channel` for any public channel the bot is not a member of. Private channels still require Slack user membership regardless of token.)
     - `admin.conversations:write` (optional — required only for cross-workspace channel connect on Enterprise Grid; same User OAuth Token also drives `search:read` when both are needed)

   - **Bot Token Scopes**:
     - `bookmarks:write` (to add bookmarks to case channels)
     - `channels:history` (to receive message events from public channels via Events API)
     - `channels:join` (optional — required only when a workspace sets `[slack] allow_existing_channel = true`; lets the bot join the existing public channel a new case is linked to. Re-install the app after adding this scope.)
     - `channels:manage` (to create, rename, and invite users to public channels)
     - `channels:read` (to list and read channel information, and receive membership events)
     - `chat:write` (to post and update action notification messages in channels)
     - `commands` (to receive slash command invocations)
     - `files:read` (to access file metadata and download files via `url_private`)
     - `groups:read` (to read private channel information and receive membership events)
     - `groups:write` (to create private channels for private cases)
     - `reactions:read` (optional — required only when a thread-mode workspace sets `[slack] reaction`; lets the bot receive `reaction_added` events for messages it can see)
     - `team:read` (to list workspaces for org-level Slack app support)
     - `usergroups:read` (to list user groups and their members for auto-invite)
     - `users:read` (to fetch user profile information including avatar images)
     - `users:read.email` (to access user email addresses from profiles)

   **Important**: User Token Scopes and Bot Token Scopes serve different purposes:
   - User Token Scopes authenticate users via OpenID Connect
   - Bot Token Scopes allow the application to manage channels, receive events, and fetch user information using the Bot Token

4. Click "Save Changes"

#### 3. Get OAuth Credentials

1. Go to **Basic Information**
2. Under **App Credentials**, you'll find:
   - **Client ID**: Copy this value
   - **Client Secret**: Click "Show" and copy this value

### Authentication Flow

#### 1. Login

The frontend automatically handles the login flow. When an unauthenticated user accesses the application:

1. The frontend's `AuthGuard` component detects unauthenticated state
2. It displays a login page with a "Sign in with Slack" button
3. Clicking the button redirects to `/api/auth/login`, passing the original location (path + query + hash) as a `return_to` query parameter so the user can be brought back after authentication. See [Post-Login Redirect](#post-login-redirect-return_to) for details.
4. The backend redirects to Slack for authentication

#### 2. Authorization

1. Slack asks the user to authorize the app
2. After authorization, Slack redirects back to `/api/auth/callback`
3. The backend:
   - Validates the OAuth callback
   - Exchanges the authorization code for user tokens
   - Creates a session token
   - Sets HTTPOnly cookies (`token_id` and `token_secret`)
   - Redirects to the original `return_to` target if one was preserved, otherwise to `/` (home page)

#### 3. Access Protected Resources

After login, authentication tokens are stored in HTTPOnly cookies:
- `token_id`: Token identifier
- `token_secret`: Token secret (for verification)

These cookies are automatically sent with subsequent requests. The backend middleware validates these tokens for all protected endpoints.

#### 4. Check Authentication Status

The frontend uses `/api/auth/me` to check authentication status:

```bash
curl https://your-server.com/api/auth/me
```

Response for authenticated users:
```json
{
  "sub": "U-xxxxxxxxx",
  "email": "user@example.com",
  "name": "User Name"
}
```

#### 5. Logout

The frontend handles logout by calling `/api/auth/logout` (POST):

```bash
curl -X POST https://your-server.com/api/auth/logout
```

The backend:
1. Deletes the session token from storage
2. Clears authentication cookies
3. Returns success response

The frontend then redirects to `/` and shows the login page.

### Post-Login Redirect (`return_to`)

When a user opens a deep link to a protected resource (e.g. `/ws/abc/cases/xyz`) without an active session, the system preserves that target across the OAuth roundtrip and brings the user back to it after they sign in.

#### How it works

1. **Frontend (`LoginPage`)**: when the user clicks **Sign in with Slack**, the page builds `return_to` from `window.location.pathname + search + hash` and appends it to `/api/auth/login` (e.g. `/api/auth/login?return_to=%2Fws%2Fabc%2Fcases%2Fxyz`).
2. **Backend (`/api/auth/login`)**: the handler validates `return_to` and, if accepted, stores the value in a separate `oauth_return_to` cookie. CSRF protection (`oauth_state`) is unchanged — the new cookie carries only the redirect target so the two responsibilities stay separate.
3. **Backend (`/api/auth/callback`)**: after verifying the OAuth `state`, the handler reads `oauth_return_to`, re-validates it, and uses it as the post-login redirect target. The cookie is cleared whether or not its value was accepted.

#### Validation rules (open-redirect protection)

`return_to` must be a same-origin relative path. The backend rejects values that:

- are empty or longer than 2048 characters
- do not start with `/`
- start with `//` (protocol-relative URL → another host)
- start with `/\` (backslash trick that some browsers normalise to `//`)
- contain control characters (anything below `0x20` or `0x7f`)
- parse as a URL with a non-empty scheme or host

Rejection is silent: the OAuth flow proceeds and the user falls back to `/` after authentication. There is no error page, so probe attempts cannot fingerprint the validator.

#### Cookie

| Cookie name | Path | HttpOnly | SameSite | Secure | MaxAge |
|---|---|---|---|---|---|
| `oauth_return_to` | `/` | yes | Lax | yes (when TLS) | 600s (10 min) |

The cookie is cleared on every callback, even when the value fails revalidation, so a stale or tampered value never persists across attempts.

#### No-auth mode

In no-auth mode the OAuth roundtrip is skipped, so there is no callback to read the cookie. The login handler honours `return_to` directly: a valid value becomes the redirect target, anything else falls back to `/`. The validator is identical to the OAuth path.

#### Notes

- **Hash fragments (`#step-3`)** survive the roundtrip because the frontend URL-encodes them into the `return_to` *query value* (so they are transmitted as data, not as the request's fragment). The backend stores the encoded value verbatim and emits it back in the `Location` header on redirect; modern browsers carry the fragment to the final navigation.
- **`return_to` is advisory, not authoritative**: it controls only where the browser lands after sign-in. Per-resource access control still happens at the GraphQL layer once the page loads.

### No-Auth Mode (Development)

For local development and testing, you can use the `--no-auth` flag to skip OAuth flow while still operating as a real Slack user:

```bash
# Optional: with a bot token, the user ID is validated against Slack
export HECATONCHEIRES_SLACK_BOT_TOKEN="xoxb-your-bot-token"
export HECATONCHEIRES_NO_AUTH="U1234567890"  # Your Slack user ID

./hecatoncheires serve
```

Or use CLI flags:
```bash
./hecatoncheires serve \
  --slack-bot-token="xoxb-your-bot-token" \
  --no-auth="U1234567890"
```

**Requirements:**
- `--slack-bot-token` is optional. When it is set, the specified user ID must exist in your Slack workspace, or the server fails to start. When it is omitted, the server starts as a placeholder user (`Test User` / `test@example.com`) with the given ID, which is enough to try the Web UI without a Slack App
- `--no-auth` takes precedence over `--slack-client-id` / `--slack-client-secret`; the OAuth settings are ignored while it is set

**How it works:**
- On startup, when `--slack-bot-token` is set, the server validates the user ID via Slack API (`users.info`)
- All requests are then automatically authenticated as that user
- No OAuth flow or cookies are required

This is useful for:
- Local development
- Testing
- CI/CD environments

### Token Management

#### Token Structure

```go
{
  "id": "unique-token-id",        // Public token identifier
  "secret": "secret-value",        // Secret for verification (masked in logs)
  "sub": "slack-user-id",         // Slack user ID
  "email": "user@example.com",    // User email
  "name": "User Name",            // User display name
  "expires_at": "2025-01-05...",  // Expiration timestamp (7 days)
  "created_at": "2024-12-29..."   // Creation timestamp
}
```

#### Token Lifecycle

1. **Creation**: On successful OAuth callback
2. **Storage**: In Firestore or Memory (depending on configuration)
3. **Caching**: In-memory cache for 5 minutes (reduces DB load)
4. **Validation**: On each request via middleware
5. **Expiration**: Automatically after 7 days
6. **Deletion**: On logout or when expired

---

## Slack Events API (Webhooks)

Slack Events API allows Hecatoncheires to receive real-time events from your Slack workspace, such as messages in channels.

### Events API Setup

#### 1. Enable Event Subscriptions

1. In your app settings, go to **Event Subscriptions**
2. Toggle **Enable Events** to **On**

#### 2. Set Request URL

Enter your webhook endpoint URL:

For local development with ngrok:
```
https://your-ngrok-id.ngrok.io/hooks/slack/event
```

For production:
```
https://your-domain.com/hooks/slack/event
```

**Note**:
- Slack requires HTTPS
- Slack will send a verification challenge immediately - your app must be running with the correct signing secret configured

#### 3. Subscribe to Bot Events

Under **Subscribe to bot events**, add the events you want to receive:

| Event | Description | Required Bot Scope |
|-------|-------------|-------------------|
| `message.channels` | Messages posted to public channels the app is in | `channels:history` |
| `app_mention` | When someone mentions your app with @app_name | (no additional scope) |
| `member_joined_channel` | When a user joins a channel | `channels:read` |
| `member_left_channel` | When a user leaves a channel | `channels:read` |

The `member_joined_channel` and `member_left_channel` events are required for **Private Case** access control. When these events fire, the application automatically syncs the channel member list to the associated case, keeping access permissions up to date.

**Reaction trigger event** (only when a thread-mode workspace sets `[slack] reaction`):

| Event | Description | Required Bot Scope |
|-------|-------------|-------------------|
| `reaction_added` | When a reaction is added to a message | `reactions:read` |

**The bot must be a member of any channel where the trigger emoji is used** — Slack only delivers `reaction_added` for messages the app can see, so a reaction in a channel the bot has not joined is never received (there is nothing to fix in configuration; invite the bot to those channels). See [Reaction-triggered case creation](#reaction-triggered-case-creation).

**Optional events** (if you need private channel or DM support):

| Event | Description | Required Bot Scope |
|-------|-------------|-------------------|
| `message.groups` | Messages posted to private channels the app is in | `groups:history` |
| `message.im` | Messages posted in direct message channels with the app | `im:history` |
| `message.mpim` | Messages posted in multi-person direct messages the app is in | `mpim:history` |

**Note**: Subscribing to optional events requires adding the corresponding scopes to **Bot Token Scopes** in OAuth & Permissions.

Click **Save Changes**

#### 4. Get Signing Secret

1. Go to **Basic Information**
2. Under **App Credentials**, find **Signing Secret**
3. Click "Show" and copy this value

This secret is used to verify that webhook requests actually come from Slack.

#### 5. Install App to Workspace

1. Go to **Install App** in the left sidebar
2. Click "Install to Workspace"
3. Review and authorize the permissions
4. After installation, you'll see **Bot User OAuth Token** - copy this value

**Note**:
- The Bot Token is needed for both fetching user avatars (authentication) and for the Events API
- The Bot Token starts with `xoxb-`

#### 6. Invite Bot to Channels

For the bot to receive messages from channels, you need to invite it:

1. Go to the Slack channel where you want to receive events
2. Type `/invite @your-bot-name`
3. The bot will now receive message events from that channel

### Webhook Event Processing

When a message is posted in a channel where your bot is present:

1. Slack sends a POST request to `/hooks/slack/event`
2. The middleware verifies the request signature using the signing secret
3. If verification succeeds, the handler:
   - Responds immediately with HTTP 200 (within 3 seconds as required by Slack)
   - Processes the event asynchronously in the background
4. The event is parsed and the message is saved to the database

#### Supported Events

Currently supported event types:

- `message` - Regular channel messages
- `app_mention` - When someone @mentions your app
- `member_joined_channel` - When a user joins a channel (triggers channel member sync for private cases)
- `member_left_channel` - When a user leaves a channel (triggers channel member sync for private cases)

Messages are stored with:
- Channel ID
- User ID and name
- Message text
- Timestamp
- Thread information (if it's a threaded message)
- File attachment metadata (if files are attached to the message)

### Thread mode (monitored channel)

When a workspace is configured with `[slack] mode = "thread"` (see
[Configuration → Slack Section](configuration.md#slack-section) and
[Case Section](configuration.md#case-section-thread-mode)), Hecatoncheires watches
the single channel named in `[slack] channel` and turns conversations into Cases.
**What starts a Case depends on `[slack] trigger`** (see
[Configuration → Case trigger](configuration.md#case-trigger-thread-mode)):

- **`instant` (default) — top-level message → Case.** Each new top-level post
  (excluding message subtypes such as edits/joins) creates a Case bound to that
  message's thread. The bot replies in-thread with a link to the web UI, and an
  LLM materializes the Case title, description, and custom fields.
- **`@mention` inside a thread that has no Case yet → Case (both modes).** In
  `instant` this is a recovery path: if a thread's root never became a Case (a
  message subtype or a bot post that `instant` creation skipped, or a thread
  predating the bot), @mentioning the bot in that thread starts a Case seeded by
  the whole thread. In `mention` mode it is the primary in-thread trigger. (A
  bot-authored mention is gated by `accept_bot`, same as everywhere else.)
- **`mention` — `@mention` in a thread → Case.** A Case is started only when the
  bot is @mentioned inside a thread that has no Case yet. Plain posts are left
  alone. The mention text and the surrounding conversation seed the same
  initialization agent as `instant`.
- **`mention` — `@mention` at the channel root → workspace agent, not a Case.**
  Mentioning the bot at the top level of the monitored channel runs the
  [workspace agent](#workspace-agent) and it replies in that mention's thread.
  Mentioning the bot again inside that thread continues the same conversation —
  it never starts a Case. (A bot-authored root mention is the exception: with
  `accept_bot = true` it still files a Case, because an app has no Slack user
  identity for the agent to act on behalf of.)
- **Thread reply → recorded on the Case.** Replies in the thread are saved to the
  Case's message history.
- **`@mention` in a thread that already has a Case → investigation agent.**
  Mentioning the bot runs a plan-and-execute agent over the Case context. It can
  answer in-thread, ask a follow-up question, update the Case fields, or close the
  Case when the thread indicates the issue is resolved. (This is independent of
  `trigger`.)

Bot-authored triggers (an intake-form app's post or mention) start a Case only
when `[slack] accept_bot = true`.

Thread-mode Cases do **not** create Actions or Drafts. Jobs run identically to
channel mode and post their output into the Case thread.

**Case changes are announced in the thread.** When a Case's title, assignees, or
board status changes — from the Web UI or from an agent tool — a single
context-block reply is posted in that Case's thread (`:pencil2: @alice changed
the case title: …`). It is deliberately quiet: one context line, and **no**
`reply_broadcast`, because every thread-mode Case shares the same monitored
channel and broadcasting would reach people watching unrelated threads. Only
real changes are posted, and a Slack failure never rolls back the change. See
[docs/user_guide.md](./user_guide.md#case-change-notifications-thread-mode) for
the full list and the exact wording.

**Setup requirements for thread mode:**

1. Subscribe to `message.channels` (and `message.groups` if the monitored channel
   is private) — see [Subscribe to Bot Events](#3-subscribe-to-bot-events).
2. Subscribe to `app_mention` — required for `trigger = "mention"`, and for the
   in-thread investigation agent in either mode.
3. Invite the bot to the monitored channel (`/invite @your-bot-name`).
4. Set `[slack] channel` to the channel **ID** (e.g. `C0123456789`), not the name.

### Workspace agent

The **workspace agent** is not bound to any single Case: it reads across and acts
on every Case the mentioning user can access (answer a question, create a Case,
update or move an existing one). Each mention thread is one continuing
conversation. Where it runs depends on the mode:

- **Channel mode** — in the optional workspace-level channel named by
  `[slack] workspace_channel`. @mentioning the bot there starts a turn.
- **Thread mode** (`trigger = "mention"`) — in the monitored channel itself. A
  channel-root @mention starts a turn and the agent replies in that mention's
  thread; further mentions in that thread continue it. No `workspace_channel` is
  needed (setting one in thread mode is a config error).

The agent acts with the **mentioning user's** permissions, so it can only see and
change Cases that user may access. It therefore runs only for a mention authored
by a Slack user — an app-authored mention has no identity to act on behalf of.

Available tools differ by mode: a thread-mode workspace has no Actions, so the
agent gets no Action tools, and it finishes a Case by moving it to a closed board
status rather than with a separate close tool. See
[Agent tools → Workspace agent cross-case tools](agent_tools.md#workspace-agent-cross-case-tools-casemulti).

The bot must be a member of the channel, and app_mention events must be delivered
(as for any mention). No new OAuth scopes are required beyond the existing
mention / `chat:write` setup. Configure it — including the optional
`[slack.workspace_agent]` prompt and its write-safety guardrail — under
[Configuration → Workspace agent](configuration.md#workspace-agent).

### Reaction-triggered case creation

A thread-mode workspace can additionally start a Case when a specific emoji is
added to a message, on top of the message / mention triggers above. Configure it
with `[slack] reaction` (thread mode only):

```toml
[slack]
mode = "thread"
channel = "C_CASES"    # the monitored channel where case threads live
reaction = "incident"  # the emoji that triggers creation (":incident:" also accepted)
```

The reaction emoji must be **unique across workspaces** so the emoji resolves to
exactly one workspace. When the trigger emoji is added:

- **Reaction inside the monitored channel** → the reacted message's thread
  becomes the Case thread directly (a reaction on a reply resolves to the thread
  root). If that thread is already a Case, the reaction is a no-op.
- **Reaction in any other channel** → the bot posts the Case in the monitored
  channel as a root message. That root message is the standard "Created a case"
  summary — identical to every other creation path (no separate "flagged for
  triage" preamble). The reaction-specific context — who flagged it and a link to
  the exact flagged message — is posted as a reply under that summary. The
  *creation dialog* (progress and any clarifying question) happens in the
  **reactor's** thread, and once the Case is created a link back to it is posted
  in that thread. Subsequent discussion continues in the monitored-channel thread
  like any thread-mode Case.

The person who added the reaction becomes the Case **reporter**. The
initialization agent treats the reacted message as the **center** of the Case: it
reads the surrounding conversation to understand *why* that message's problem,
complaint, or unmet need was raised, but keeps the Case anchored to that concern
rather than letting a tangential thread topic take over. When the reacted
message's Slack link is available it is preserved in the Case description, so the
Case always points back to its source.

**Setup requirements for the reaction trigger:**

1. Add the `reactions:read` Bot Token Scope and subscribe to the `reaction_added`
   event — see [Subscribe to Bot Events](#3-subscribe-to-bot-events).
2. **Invite the bot to every channel where the trigger emoji may be used.** Slack
   delivers `reaction_added` only for messages the bot can see, so a reaction in a
   channel the bot has not joined is never received.

---

## Slack Interactivity (Action Notifications)

When an action is created in Hecatoncheires, a notification message is automatically posted to the associated case's Slack channel. This message includes interactive buttons that allow users to update the action status directly from Slack.

### How It Works

1. When an action is created, if the associated case has a Slack channel, an Action card is posted
2. The Action card has the shape of "top-level text + one attachment carrying Block Kit content" rather than top-level Block Kit. This is required so that `reply_broadcast=true` thread replies (status / assignee / step events — see below) render the parent Action card excerpt in the channel view; top-level Block Kit collapses Slack's preview to a generic "a thread" link
3. The top-level text is the title line: a fixed prefix emoji (📌) signals "this row is an Action card", followed by the bold linked title (e.g. `📌 *<webui-url|Investigate login failures>*`). The "Action:" literal was dropped and the per-status emoji was removed from the title — status is communicated via the attachment side-bar color and the Status select element
4. The attachment carries: an optional description Section block (only when the Action has a description), and an Actions block with Status and Assignee selects. The attachment `color` tracks the current status (`ActionStatusDefinition.SlackColor`, which maps preset names like `active` / `blocked` / `success` to hex), so the side-bar gives status a glance-level read
5. Slack auto-appends "Added by {bot name}" as the attachment footer. This is Slack-side attribution and cannot be suppressed via API; treat it as part of the card layout
6. Status / assignee changes from the web UI or Slack interactivity refresh the same message, so the title link, description, attachment color, and select state stay in sync

In addition, Action change events (status / assignee / title edits) and
ActionStep CRUD events (add / remove / done / reopen / rename) post a
context-block thread reply to the Action's primary Slack message — see
[docs/user_guide.md](./user_guide.md#lifecycle-events) for the full list of step events
and notification text.

Status changes, assignee changes, and every Step CRUD event are additionally
posted with `reply_broadcast=true` so they appear inside the Action's
thread AND in the parent Case channel ("Also sent to #channel"). This lets
channel watchers see progress on important transitions without expanding
every thread. Title-only edits and `ARCHIVED` / `UNARCHIVED` events stay
thread-only. The broadcast set is centralised in `broadcastableActionEvents`
(`pkg/usecase/action_broadcast.go`); adding a kind there enables broadcasting
from every notify path at once.

A comment written on an Action from the web UI also posts a context-block
thread reply, broadcast the same way (`actionCommentBroadcasts`, in the same
file — a comment is an `ActionComment`, not an `ActionEventKind`, so it cannot
be an entry in that map). The reply carries only the author, a deep link to the
comment in the web UI, and a 100-character excerpt of the body; the comment
itself is never reproduced as a Slack message, and editing or deleting one
posts nothing. See
[docs/user_guide.md](./user_guide.md#slack-notification) for the message shape
and the deep-link format.

### Interactivity Setup

#### 1. Enable Interactivity

1. In your Slack app settings, go to **Interactivity & Shortcuts**
2. Toggle **Interactivity** to **On**

#### 2. Set Request URL

Enter your interactivity endpoint URL:

For local development with ngrok:
```
https://your-ngrok-id.ngrok.io/hooks/slack/interaction
```

For production:
```
https://your-domain.com/hooks/slack/interaction
```

**Note**:
- Slack requires HTTPS
- The interactivity endpoint uses the same Slack signing secret as the Events API webhook for request verification
- Your app must be running with the correct signing secret configured

#### 3. Add Required Bot Scopes

The bot token must have the `chat:write` scope to post and update messages:

1. In your app settings, go to **OAuth & Permissions**
2. Under **Bot Token Scopes**, add:
   - `chat:write` (required for posting and updating messages in channels)
3. Click **Save Changes**
4. **Reinstall the app** to apply the new scope

### Message Format

The Action card consists of:

- **Top-level text (title line)**: `📌 *<webui-url|Title>*` — fixed prefix emoji + bold linked title. This text is also what Slack picks up as the parent-message excerpt for broadcasted thread replies
- **Attachment side-bar color**: hex value derived from the status definition's `Color` (preset name resolved via `ActionStatusDefinition.SlackColor`, or hex value passed through verbatim)
- **Attachment Section block (optional)**: Action description, present only when the Action has a non-empty description
- **Attachment Actions block**: Status select + Assignee select, side-by-side
- **Attachment footer**: "Added by {bot name}" — Slack-injected attribution, not configurable

The message is automatically updated when the Action is modified (title, assignees, status, description, etc.) from the web UI or Slack interactivity. The attachment color follows status transitions.

### Requirements

- The case must have a Slack channel (`slackChannelID` must be set)
- The bot must be a member of the channel
- The bot token must have `chat:write` scope
- The signing secret must be configured for interaction verification
- Slack message posting is best-effort: if it fails, action creation still succeeds

---

## Slack Slash Commands (Case Creation & Editing)

Slack slash commands allow users to create and edit cases directly from Slack without opening the web UI. The slash command behaves differently depending on the channel context:

- **In a case channel**: Opens an edit modal with the current case values prefilled
- **In any other channel**: Opens a case creation modal

### How It Works

#### Case Creation (non-case channels)

1. User types a slash command (e.g., `/create-case`) in a regular Slack channel
2. Slack sends a request to Hecatoncheires
3. A Block Kit modal opens with the case creation form
4. User fills in the form and submits
5. A new case is created and a confirmation message is posted to the channel

#### Case Editing (case channels)

1. User types the slash command inside a case's dedicated Slack channel
2. Hecatoncheires detects that the channel is linked to an existing case
3. A Block Kit modal opens with all current values prefilled (title, description, and custom fields)
4. User modifies the values and submits
5. The case is updated and a confirmation message is posted to the channel

**Notes:**
- Private case access control is enforced: non-members of a private case channel receive an ephemeral error message
- Assignees are preserved during edit (they are managed separately in the web UI)
- If the title is changed, the Slack channel is automatically renamed to match

#### Workspace Selection

The behavior depends on how many workspaces are configured and whether a workspace ID is specified in the URL:

- **Workspace ID in URL** (e.g., `/hooks/slack/command/risk`): Opens the case creation modal directly for that workspace (unless the channel already has a linked case)
- **Single workspace configured**: Opens the case creation modal automatically
- **Multiple workspaces configured**: Shows a workspace selection modal first, then the case creation modal

### Slash Command Setup

#### 1. Create a Slash Command in Slack

1. In your Slack app settings, go to **Slash Commands**
2. Click **Create New Command**
3. Configure the command:
   - **Command**: Enter the command name (e.g., `/create-case`)
   - **Request URL**: Set to your slash command endpoint:
     ```
     https://your-domain.com/hooks/slack/command
     ```
     Or to target a specific workspace:
     ```
     https://your-domain.com/hooks/slack/command/{workspace_id}
     ```
   - **Short Description**: e.g., "Create a new case"
4. Click **Save**

You can create multiple slash commands targeting different workspaces. For example:
- `/create-risk` → `https://your-domain.com/hooks/slack/command/risk`
- `/create-incident` → `https://your-domain.com/hooks/slack/command/incident`

#### 2. Enable Interactivity (Required)

Slash commands use Block Kit modals, which require **Interactivity** to be enabled. If you have already configured [Slack Interactivity](#slack-interactivity-action-notifications), this is already done. Otherwise:

1. Go to **Interactivity & Shortcuts** in your Slack app settings
2. Toggle **Interactivity** to **On**
3. Set **Request URL** to:
   ```
   https://your-domain.com/hooks/slack/interaction
   ```
4. Click **Save Changes**

#### 3. Required Bot Scopes

The bot token must have the `chat:write` scope to post confirmation messages after case creation. Add this scope in **OAuth & Permissions** → **Bot Token Scopes** if not already present.

### Custom Fields in Modal

The case creation modal dynamically includes input fields based on the workspace's field configuration (defined in TOML config). Supported field types:

| Field Type | Modal Input |
|------------|------------|
| `text` | Plain text input |
| `number` | Number input |
| `select` | Single-select dropdown |
| `multi_select` | Multi-select dropdown |
| `user` | Slack user selector |
| `multi_user` | Multi-user selector |
| `date` | Date picker |
| `url` | URL text input |

### Requirements

- Slack signing secret must be configured (for request verification)
- Bot token must have `chat:write` scope (for posting confirmation messages)
- Interactivity must be enabled in the Slack app
- At least one workspace must be configured

---

## Automatic Risk Channel Creation

Hecatoncheires can automatically create dedicated Slack channels for each risk when it is registered. This feature helps teams collaborate and discuss specific risks in dedicated channels.

### How It Works

When a new case is created through the GraphQL API:

1. A Slack channel is automatically created with a standardized name
2. If the case is marked as **Private**, the channel is created as a **private channel**; otherwise, it is a public channel
3. The channel ID is stored with the case in the `slackChannelID` field
4. If channel creation fails, the case creation is rolled back (transactional)

When a case is updated and its name changes:

1. The associated Slack channel is automatically renamed to match the new case name
2. The channel ID remains the same

### Private Case Channel Behavior

For private cases, additional behavior applies:

- The Slack channel is created as a **private channel**, restricting visibility to invited members only
- Channel member IDs are synced to the case and used for **access control** — only channel members can view the case, its actions, and assist logs via the API and UI
- Member sync happens automatically when `member_joined_channel` or `member_left_channel` events are received, or manually via the **Sync** button on the case detail page
- Bot users are automatically filtered out from the stored member list

### Auto-Invite

Hecatoncheires can automatically invite specified users and user group members to case channels upon creation. This is configured in the `[slack.invite]` section of the TOML configuration file.

```toml
[slack.invite]
users = ["U12345678", "U87654321"]
groups = ["S0614TZR7", "@security-response"]
```

- **`users`**: A list of Slack user IDs to invite directly
- **`groups`**: A list of Slack user group IDs or `@`-prefixed handle names. Group members are resolved and invited automatically

When a case is created:
1. The creator and assignees are invited first
2. Auto-invite users are added to the invite list
3. Group identifiers are resolved to member lists (`@`-prefixed handle names are looked up via `usergroups.list`)
4. All users are deduplicated before the invitation API call

Group resolution failures are logged but do not block case creation. The `usergroups:read` bot scope is required.

### Channel Naming Convention

Channels are named using the format: `{prefix}-{risk-id}-{normalized-risk-name}`

For example:
- Risk ID: `42`
- Risk Name: "SQL Injection in User Auth"
- Prefix: `risk` (from the workspace's `channel_prefix`; defaults to the workspace ID when unset)
- Result: `#risk-42-sql-injection-in-user-auth`

With a custom prefix:
- Prefix: `incident`
- Result: `#incident-42-sql-injection-in-user-auth`

Japanese characters are supported:
- Risk Name: "認証システムの脆弱性"
- Result: `#risk-42-認証システムの脆弱性`

### Channel Name Normalization

Risk names are automatically normalized to comply with Slack's channel naming rules:

- Uppercase letters → converted to lowercase
- Spaces → replaced with hyphens
- Special characters (slashes, periods, commas, etc.) → removed
- Japanese characters (hiragana, katakana, kanji) → preserved
- Maximum length: 80 characters (truncated if longer)

### Customizing the Channel Prefix

The channel name prefix is a per-workspace setting: set `channel_prefix` in the `[slack]` section of the workspace's TOML configuration file. If it is not specified, the workspace ID is used as the prefix.

```toml
[slack]
channel_prefix = "incident"
```

This allows you to organize channels by different categories per workspace (e.g., `incident-*`, `security-*`, `vulnerability-*`). See [configuration.md](./configuration.md#slack-section) for the full `[slack]` section reference.

### Required Bot Permissions

For automatic channel creation and full Slack integration, the bot token must have the following scopes:

- `bookmarks:write` - Add bookmarks to case channels
- `channels:history` - Receive message events from public channels via Events API
- `channels:join` - Join an existing public channel that a new case is linked to (optional; only with `[slack] allow_existing_channel`)
- `channels:manage` - Create, rename, and invite users to public channels
- `channels:read` - List and read channel information, receive membership events
- `chat:write` - Post and update action notification messages in channels
- `commands` - Receive slash command invocations
- `files:read` - Access file metadata and download files attached to messages
- `groups:read` - Read private channel information, receive membership events for private channels
- `groups:write` - Create private channels for private cases
- `team:read` - List workspaces for org-level Slack app support
- `usergroups:read` - List user groups and their members (for auto-invite feature)
- `users:read` - Fetch user profile information (name, avatar)
- `users:read.email` - Access user email addresses from profiles

Add these scopes in **OAuth & Permissions** → **Bot Token Scopes** in your Slack app settings.

### Configuration

Enable automatic channel creation by providing a bot token:

```bash
export HECATONCHEIRES_SLACK_BOT_TOKEN="xoxb-your-bot-token"
```

Or using a CLI flag:

```bash
./hecatoncheires serve --slack-bot-token="xoxb-your-bot-token"
```

The channel name prefix is configured per workspace via `[slack] channel_prefix` in the TOML configuration file (defaults to the workspace ID), not via a CLI flag.

If no bot token is provided, risks will be created without Slack channels (the `slackChannelID` field will be empty).

### Examples

**Example 1: Default configuration**
```bash
export HECATONCHEIRES_SLACK_BOT_TOKEN="xoxb-..."
./hecatoncheires serve
```

Creating a risk named "XSS Vulnerability in Dashboard" (ID: 5) will create channel: `#risk-5-xss-vulnerability-in-dashboard`

**Example 2: Custom prefix**

Set the prefix in the workspace's TOML configuration:

```toml
[slack]
channel_prefix = "sec"
```

Creating a risk named "Data Leak in API" (ID: 12) will create channel: `#sec-12-data-leak-in-api`

**Example 3: Japanese risk names**
```bash
export HECATONCHEIRES_SLACK_BOT_TOKEN="xoxb-..."
./hecatoncheires serve
```

Creating a risk named "データベースのSQLインジェクション" (ID: 7) will create channel: `#risk-7-データベースのsqlインジェクション`

---

## Enterprise Grid (Org-Level App) Setup

Hecatoncheires supports Slack Enterprise Grid with org-level app installation. This allows a single app to operate across all workspaces in the organization.

### Overview

When using an org-level Slack app:

- **Auto-detection**: At startup, hecatoncheires calls `auth.test` and checks for `enterprise_id` to automatically detect whether the app is org-level or workspace-level
- **Channel creation**: Channels are created in the workspace specified by `slack.team_id` in the TOML config
- **Cross-workspace channel connect**: When a case is created from a slash command in a different workspace than the configured `slack.team_id`, the case channel is automatically connected to the source workspace via `admin.conversations.setTeams`, making the channel visible to users in both workspaces. This requires a **User OAuth Token** (`xoxp-`) with `admin.conversations:write` scope, configured via `--slack-user-oauth-token`. If not configured, the creator receives an ephemeral notification with instructions for manual channel connection.
- **User sync**: The background user refresh worker automatically discovers all workspaces via `auth.teams.list` and fetches users from each workspace
- **Backward compatible**: Existing workspace-level app configurations work without any changes

### Step 1: Create an Org-Level Slack App

1. Go to [https://api.slack.com/apps](https://api.slack.com/apps)
2. Create a new app or use an existing one
3. Under **Org Level Apps** (in the app settings sidebar), enable org-level distribution
4. Configure the same OAuth scopes as described in the [authentication setup](#2-configure-oauth--permissions)

### Step 2: Install the App to All Workspaces

The app must be installed to each workspace where you want to fetch users.

1. Go to the Enterprise Grid admin console: `https://app.slack.com/manage/{ENTERPRISE_ID}`
2. Click **Manage Organization** (top-right)
3. Navigate to **Integrations** in the left sidebar
4. Click **Manage installed apps**
5. Find your app and click on it
6. On the **Installations** tab, install the app to all relevant workspaces

> **Important**: The user refresh worker uses `auth.teams.list` to discover workspaces, which only returns workspaces where the app is installed. If the app is only installed in one workspace, only users from that workspace will be fetched.

### Step 3: Find Workspace Team IDs

Each workspace has a Team ID (starts with `T`). You need this for the `slack.team_id` setting in your TOML config.

**From the Enterprise admin console:**

1. Go to `https://app.slack.com/manage/{ENTERPRISE_ID}`
2. Click **Manage Organization**
3. Navigate to **Workspaces** in the left sidebar
4. Click on a workspace
5. The URL will contain the Team ID: `https://app.slack.com/manage/{ENTERPRISE_ID}/workspaces/{TEAM_ID}/settings`

**From the startup log:**

At startup, hecatoncheires logs the detected Team ID:
```
INFO Detected org-level Slack app enterprise_id=E02GQTQ0E48 team_id=T2G71SMA8
```

### Step 4: Configure Workspace TOML

Add `slack.team_id` to each workspace TOML configuration. This tells hecatoncheires which Slack workspace to create channels in for each hecatoncheires workspace.

```toml
[workspace]
id = "risk"
name = "Risk Management"

[slack]
channel_prefix = "risk"
team_id = "T2G71SMA8"  # Required for org-level apps

[slack.invite]
users = ["U12345678"]
```

### Validation Rules

At startup, hecatoncheires validates the `slack.team_id` configuration:

| App Type | `slack.team_id` set | `slack.team_id` empty |
|----------|--------------------|-----------------------|
| **Org-Level App** | OK | **Startup error** (required) |
| **WS-Level App** | Must match `auth.test` team_id, otherwise **startup error** | OK (default) |

- For org-level apps, every workspace config MUST have `slack.team_id`
- For workspace-level apps, `slack.team_id` is optional. If set, it must match the bot's actual workspace
- If no bot token is configured, validation is skipped entirely

### User Refresh Behavior

The background user refresh worker behaves differently based on the app type:

- **Org-Level App**: Calls `auth.teams.list` to discover all installed workspaces, then calls `users.list` with each Team ID. Users are deduplicated across workspaces
- **WS-Level App**: Calls `users.list` without a Team ID (single workspace, same as before)

### Enterprise Grid Troubleshooting

#### Only users from one workspace are fetched

Check the startup log for `fetched users from all workspaces`:
```
INFO fetched users from all workspaces team_count=11 unique_user_count=1500
```

If `team_count` is 1, the app is likely only installed in one workspace. Install it to all workspaces via the Enterprise admin console (see [Step 2](#step-2-install-the-app-to-all-workspaces)).

#### Startup error: "org-level Slack app requires slack.team_id"

All workspace TOML configs must have `slack.team_id` set when using an org-level app. See [Step 3](#step-3-find-workspace-team-ids) for how to find your Team IDs.

#### Startup error: "slack.team_id does not match the bot's workspace"

You have `slack.team_id` set in a workspace config, but the app is a workspace-level (not org-level) app. Either remove the `slack.team_id` setting or switch to an org-level app.

---

## Message Storage and Retrieval

Messages received from Slack webhooks are stored in Firestore and can be queried via GraphQL (when implemented) or directly through the repository layer.

### Storage Structure

Messages are stored in Firestore using a subcollection structure:

```
slack_channels/{channelID}
  - channel_id: string
  - team_id: string
  - last_message_at: timestamp
  - message_count: integer

  messages/{messageID}
    - id: string (message timestamp)
    - thread_ts: string (thread timestamp, if threaded)
    - user_id: string
    - user_name: string
    - text: string
    - event_ts: string
    - files: array (file attachment metadata, may be empty)
      - id: string (Slack file ID)
      - name: string (file name)
      - mimetype: string (MIME type)
      - filetype: string (Slack file type code)
      - size: int (file size in bytes)
      - url_private: string (Slack authenticated access URL)
      - permalink: string (Slack file permalink)
      - thumb_url: string (thumbnail URL, if available)
    - created_at: timestamp
```

This structure allows efficient querying and pagination per channel.

### Message Lifecycle

1. **Reception**: Webhook receives event from Slack
2. **Verification**: Signature is verified
3. **Async Processing**: Event is processed in background
4. **Storage**: Message is saved to Firestore
5. **Metadata Update**: Channel metadata is updated (last message time, count)

### Future Features

- GraphQL queries for message retrieval
- Threaded conversation display
- User mention notifications
- Message reactions support

> **Note on agent message search**: workspace-wide Slack message search is implemented as the AI agent tool `slack__search_messages` (in `pkg/agent/tool/slack`) and uses the Slack `search.messages` API. That API is **User-token-only**, so a Slack User OAuth Token (`xoxp-...`) with the `search:read` scope is required. Configure it via `HECATONCHEIRES_SLACK_USER_OAUTH_TOKEN`. When the User OAuth Token is not configured, the search tool is silently omitted from the agent's tool set.

> **Note on agent message retrieval (`slack__get_messages`)**: the agent's bulk message-fetch tool uses `conversations.replies` / `conversations.history`. Per the Slack API contract ("Only user tokens can access public channels they are not in"), Bot tokens can only read channels the bot has been invited to and return `not_in_channel` otherwise. When the same User OAuth Token also has the `channels:history` scope, the tool routes the call through the User token, which means **public** channels are readable even without bot membership. Private channels still require Slack user membership regardless of token; this scope does not change that. If `channels:history` is not granted on the User OAuth Token, the tool transparently falls back to the Bot token (existing behaviour).

---

## Security Considerations

### Production Deployment

1. **HTTPS Required**: Always use HTTPS in production
   - OAuth callbacks require HTTPS
   - Webhook endpoints require HTTPS
   - Cookies are set with `Secure` flag when using HTTPS
   - Token secrets are transmitted securely

2. **Set Base URL**: Use your actual domain
   ```bash
   export HECATONCHEIRES_BASE_URL="https://your-domain.com"
   ```
   The callback URL (`/api/auth/callback`) is automatically appended

3. **Signing Secret Verification**:
   - All webhook requests are verified using HMAC-SHA256
   - Timestamp verification prevents replay attacks (5-minute window)
   - Requests with invalid signatures are rejected with HTTP 401

4. **Secure Token Storage**:
   - Tokens are stored with masked secrets in logs
   - HTTPOnly cookies prevent XSS attacks
   - SameSite=Lax for CSRF protection

5. **Token Expiration**:
   - Session tokens expire after 7 days
   - Expired tokens are automatically deleted
   - Token cache TTL: 5 minutes

6. **Error Handling**:
   - Backend returns HTTP 401 for unauthenticated requests
   - Frontend handles authentication redirects
   - Backend never redirects to login page (frontend responsibility)

### Firestore Security Rules

If using Firestore for storage, configure security rules:

```javascript
rules_version = '2';
service cloud.firestore {
  match /databases/{database}/documents {
    // Tokens collection - server-side access only
    match /tokens/{tokenId} {
      allow read, write: if false;
    }

    // Slack messages - server-side access only
    match /slack_channels/{channelId} {
      allow read, write: if false;
      match /messages/{messageId} {
        allow read, write: if false;
      }
    }
  }
}
```

---

## Permissions Reference

### Bot Token Scopes

These scopes are required for the Bot User OAuth Token (`xoxb-...`):

| Scope | Slack API Method | Purpose | Code Location |
|-------|-----------------|---------|---------------|
| `bookmarks:write` | `bookmarks.add` | Add bookmarks to case channels | `pkg/service/slack/client.go` |
| `channels:history` | Events API | Receive `message.channels` events from public channels | Webhook handler |
| `admin.conversations:write` (User token) | `admin.conversations.setTeams` | Connect channels across workspaces in Enterprise Grid (requires User OAuth Token, org-level only) | `pkg/service/slack/admin_client.go` |
| `channels:join` | `conversations.join` | Make the bot a member of the existing public channel a new case is linked to (`[slack] allow_existing_channel`) | `pkg/service/slack/client.go` |
| `channels:read` / `groups:read` | `users.conversations` | List the channels the acting user belongs to that the bot can see, as candidates for linking a new case (`[slack] allow_existing_channel`) | `pkg/service/slack/client.go` |
| `channels:manage` | `conversations.create` | Create new public Slack channels for cases | `pkg/service/slack/client.go` |
| `channels:manage` | `conversations.rename` | Rename Slack channels when case name changes | `pkg/service/slack/client.go` |
| `channels:manage` | `conversations.invite` | Invite users to case channels | `pkg/service/slack/client.go` |
| `channels:read` | `conversations.list` | List public channels the bot has joined | `pkg/service/slack/client.go` |
| `channels:read` | `conversations.info` | Get channel name and info (with caching); also drives the draft-mode planner's `# Channel context` prompt section (topic, purpose, privacy, member count) | `pkg/service/slack/client.go` |
| `channels:read` | Events API | Receive `member_joined_channel` / `member_left_channel` events | `pkg/usecase/slack.go` |
| `chat:write` | `chat.postMessage` | Post action notification messages to channels | `pkg/service/slack/client.go` |
| `chat:write` | `chat.update` | Update action notification messages after button clicks | `pkg/service/slack/client.go` |
| `commands` | Slash commands | Receive slash command invocations (e.g., case creation) | `pkg/controller/http/slack_command.go` |
| `files:read` | Events API | Access file metadata attached to messages via `url_private` | Webhook handler |
| `groups:read` | `conversations.info` | Read private channel info (topic, purpose, etc.) and receive membership events; also drives the draft-mode planner's channel-context prompt section for private channels | `pkg/service/slack/client.go` |
| `groups:write` | `conversations.create` | Create private Slack channels for private cases | `pkg/service/slack/client.go` |
| `team:read` | `auth.teams.list` | List workspaces for org-level Slack app support | `pkg/service/slack/client.go` |
| `usergroups:read` | `usergroups.list` | List user groups for handle name resolution (auto-invite) | `pkg/service/slack/client.go` |
| `usergroups:read` | `usergroups.users.list` | Get user group members (auto-invite) | `pkg/service/slack/client.go` |
| `users:read` | `users.info` | Fetch user profile (name, avatar) | `pkg/service/slack/client.go` |
| `users:read` | `users.list` | List all non-deleted, non-bot users in workspace | `pkg/service/slack/client.go` |
| `users:read.email` | `users.info`, `users.list` | Access user email addresses from profiles | `pkg/service/slack/client.go` |

### User Token Scopes

These scopes apply to the User OAuth Token (`xoxp-...`):

| Scope | Required for | Purpose | Code Location |
|-------|--------------|---------|---------------|
| `openid` | OIDC sign-in (always) | OpenID Connect authentication flow | `pkg/usecase/auth.go` |
| `profile` | OIDC sign-in (always) | Access user's name | `pkg/usecase/auth.go` |
| `email` | OIDC sign-in (always) | Access user's email address | `pkg/usecase/auth.go` |
| `search:read` | Agent message search (optional) | `search.messages` — workspace-wide message search backing `slack__search_messages` agent tool | `pkg/agent/tool/slack/search_client.go` |
| `channels:history` | Agent message retrieval — public-channel reach without bot membership (optional) | `conversations.replies` / `conversations.history` — backing `slack__get_messages` agent tool. With this scope on the User token, public channels are readable without the bot being invited (Slack-side contract). Without it, the tool falls back to the Bot token and returns `not_in_channel` for non-member channels. Private channels still require Slack user membership. | `pkg/agent/tool/slack/search_client.go` |
| `admin.conversations:write` | Enterprise Grid cross-workspace channel connect (optional) | `admin.conversations.setTeams` — extends a channel to additional workspaces | `pkg/service/slack/admin_client.go` |

> The OIDC sign-in scopes are configured on the per-user OAuth flow; the bot/admin/search scopes are configured under **OAuth & Permissions → User Token Scopes** and require an app re-install when added. The same User OAuth Token (`HECATONCHEIRES_SLACK_USER_OAUTH_TOKEN`) backs both the search and admin clients.

### Event Subscriptions

These bot events must be subscribed to in the Slack app settings:

| Event | Required Bot Scope | Currently Handled |
|-------|-------------------|-------------------|
| `message.channels` | `channels:history` | Yes |
| `app_mention` | (none) | Yes |
| `member_joined_channel` | `channels:read` (or `groups:read` for private channels) | Yes |
| `member_left_channel` | `channels:read` (or `groups:read` for private channels) | Yes |
| `message.groups` | `groups:history` | Optional |
| `message.im` | `im:history` | Optional |
| `message.mpim` | `mpim:history` | Optional |

### Other Requirements

| Requirement | Purpose |
|------------|---------|
| Signing Secret | HMAC-SHA256 verification of webhook requests |
| OAuth Client ID | OpenID Connect authentication |
| OAuth Client Secret | OpenID Connect token exchange |
| Redirect URL | OAuth callback (`${BASE_URL}/api/auth/callback`) |
| Request URL (Events) | Events API webhook endpoint (`${BASE_URL}/hooks/slack/event`) |
| Request URL (Interactivity) | Interactivity endpoint (`${BASE_URL}/hooks/slack/interaction`) |

---

## API Endpoints

### Authentication Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/auth/login` | GET | Initiates OAuth flow (redirects to Slack) |
| `/api/auth/callback` | GET | OAuth callback handler (internal use) |
| `/api/auth/logout` | POST | Logs out and deletes token |
| `/api/auth/me` | GET | Returns current user info |
| `/api/auth/user-info` | GET | Returns Slack user profile (requires `user` query param) |

### Webhook Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/hooks/slack/event` | POST | Receives Slack Events API webhooks |
| `/hooks/slack/interaction` | POST | Receives Slack interactive component payloads (button clicks, modal submissions) |
| `/hooks/slack/command` | POST | Receives Slack slash command invocations (opens case creation modal) |
| `/hooks/slack/command/{ws_id}` | POST | Receives Slack slash command invocations for a specific workspace |

All webhook endpoints require valid Slack signature verification.

---

## Environment Variables Reference

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `HECATONCHEIRES_BASE_URL` | Yes* | - | Base URL of the application (e.g., `https://your-domain.com`). No trailing slash. |
| `HECATONCHEIRES_SLACK_CLIENT_ID` | Yes* | - | Slack OAuth client ID |
| `HECATONCHEIRES_SLACK_CLIENT_SECRET` | Yes* | - | Slack OAuth client secret |
| `HECATONCHEIRES_SLACK_BOT_TOKEN` | No*** | - | Slack Bot User OAuth Token (starts with `xoxb-`) |
| `HECATONCHEIRES_SLACK_USER_OAUTH_TOKEN` | No | - | Slack User OAuth Token (starts with `xoxp-`) for cross-workspace channel connect in Enterprise Grid. Requires `admin.conversations:write` scope. |
| `HECATONCHEIRES_SLACK_SIGNING_SECRET` | Yes** | - | Slack Events API signing secret |
| `HECATONCHEIRES_NO_AUTH` | No | - | Slack user ID for no-auth mode (development only) |

\* Required for OAuth mode. The callback URL is automatically constructed as `${BASE_URL}/api/auth/callback`.

\** Required only if you want to enable Slack webhook integration. Without this, webhook endpoints will not be enabled.

\*** Optional with `HECATONCHEIRES_NO_AUTH`: when it is set, the no-auth user ID is validated against Slack; when it is not, no-auth mode runs as a placeholder test user.

For local development with ngrok:
1. Start ngrok: `ngrok http 8080`
2. Set `HECATONCHEIRES_BASE_URL` to the HTTPS URL provided by ngrok (without trailing slash)
3. Update both OAuth redirect URL and Events request URL in Slack app settings

---

## Troubleshooting

### Authentication Issues

#### Login fails with "invalid_client"
- Verify `HECATONCHEIRES_SLACK_CLIENT_ID` and `HECATONCHEIRES_SLACK_CLIENT_SECRET`
- Check that the client secret hasn't been regenerated in Slack

#### Callback fails with "redirect_uri_mismatch"
- Ensure the callback URL in Slack app settings exactly matches `${HECATONCHEIRES_BASE_URL}/api/auth/callback`
- Check for trailing slashes (BASE_URL should not have trailing slash)
- Verify you're using HTTPS

#### Token verification fails
- Check system time synchronization (JWT verification is time-sensitive)
- Verify network access to `https://slack.com/.well-known/openid-configuration`

#### Authentication not working
- Verify all required environment variables are set:
  - `HECATONCHEIRES_BASE_URL`
  - `HECATONCHEIRES_SLACK_CLIENT_ID`
  - `HECATONCHEIRES_SLACK_CLIENT_SECRET`
- Check for typos in variable names
- Ensure values are not empty strings
- Verify `BASE_URL` doesn't have a trailing slash

#### No-auth mode fails to start
- When `HECATONCHEIRES_SLACK_BOT_TOKEN` is set, the server looks the no-auth user ID up in Slack and stops with `failed to validate Slack user` if it cannot. Ensure the token is valid and the user ID exists in your Slack workspace
- To start without any Slack lookup, unset `HECATONCHEIRES_SLACK_BOT_TOKEN`; no-auth mode then runs as a placeholder test user
- `--slack-client-id` / `--slack-client-secret` do not prevent startup: `--no-auth` takes precedence and they are ignored

### Webhook Issues

#### Webhook verification fails
- Verify `HECATONCHEIRES_SLACK_SIGNING_SECRET` is correct
- Ensure the signing secret matches the one in Slack app settings
- Check server logs for signature verification errors

#### Events not being received
- Verify the app is installed to the workspace
- Check that the bot is invited to the channels (`/invite @bot-name`)
- Verify Event Subscriptions are enabled in Slack app settings
- Check that you've subscribed to the correct bot events
- Ensure the request URL is correctly set in Slack

#### Request URL verification fails
- Make sure the server is running with correct signing secret
- Verify the endpoint is accessible from the internet (check ngrok URL)
- Check server logs for incoming requests

### General Issues

#### Understanding error messages in Slack

When a Slack-triggered operation fails (a mention, a reaction, a case draft), the
bot replies with a structured message instead of a generic "please try again":

```
⚠️ <what happened>
*Technical note*: <short cause> (<slack error code / reason>)
*What you can do*: <how to fix it, or that it can't be fixed by retrying>
ref: `<8-hex id>`
```

- **What you can do** tells you whether it is actionable on your side. Common
  cases: invite the bot to the destination channel (`not_in_channel`), ask an
  admin to grant a missing scope or re-authorize the app (`missing_scope` /
  `invalid_auth`), or fill required fields via **Edit** (field validation).
- **ref** is a correlation id. The same id is attached to the server log entry
  (and, for unexpected errors, to the Sentry event) for that failure. Quote it
  when asking an operator to investigate — they can grep logs / Sentry by
  `ref_id`.
- Operator/user-actionable errors (permission, validation) are logged but do
  **not** page Sentry; unexpected errors do. So a Slack error with a clear "What
  you can do" is usually a configuration fix, not a bug report.

#### User avatars not displaying
- Verify `HECATONCHEIRES_SLACK_BOT_TOKEN` is set
- Ensure the app is installed to your workspace
- Verify the bot token has `users:read` scope
- Check that the bot token starts with `xoxb-`

---

## See Also

- [Configuration Guide](./configuration.md) - TOML config file and field definitions
- [CLI Reference](./cli.md) - CLI flags
- [Integrations Guide](./integrations/README.md) - Notion, GitHub, and Jira integrations
- [User Guide](./user_guide.md) - Action steps, drafts, case import, and Slack-driven workflows
