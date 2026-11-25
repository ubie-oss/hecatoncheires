package http

import (
	"context"
	"net/http"
	"strings"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/auth"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/errutil"
	"golang.org/x/net/http/httpguts"
	"google.golang.org/api/idtoken"
)

// MCPIdentity configures request authentication and workspace access using
// Google ID tokens and the synced Slack user directory.
type MCPIdentity struct {
	Header          string
	Audience        string
	Users           interfaces.SlackUserRepository
	WorkspaceAccess interfaces.WorkspaceAuthorizer
}

type mcpIdentity struct {
	MCPIdentity
	validate func(context.Context, string, string) (*idtoken.Payload, error)
}

// WithMCPIdentity requires a verified Slack identity for all MCP requests.
func WithMCPIdentity(config MCPIdentity) MCPOption {
	return withMCPIdentity(config, idtoken.Validate)
}

func withMCPIdentity(config MCPIdentity, validate func(context.Context, string, string) (*idtoken.Payload, error)) MCPOption {
	if !httpguts.ValidHeaderFieldName(config.Header) || strings.TrimSpace(config.Audience) == "" || config.Users == nil || config.WorkspaceAccess == nil || validate == nil {
		panic("MCP identity requires a valid header, audience, user directory, and workspace authorizer")
	}
	return func(h *mcpHandler) {
		h.identity = &mcpIdentity{MCPIdentity: config, validate: validate}
	}
}

func (i *mcpIdentity) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values(i.Header)
		// Consume the credential before policy input is captured or later
		// middleware reports the request. Request context copies share Header.
		r.Header.Del(i.Header)
		if len(values) != 1 {
			http.Error(w, "Google ID token required", http.StatusUnauthorized)
			return
		}
		value := strings.TrimSpace(values[0])
		if scheme, token, ok := strings.Cut(value, " "); ok && strings.EqualFold(scheme, "Bearer") {
			value = strings.TrimSpace(token)
		}
		if value == "" || strings.ContainsAny(value, ", \t") {
			http.Error(w, "Invalid Google ID token", http.StatusUnauthorized)
			return
		}

		ctx := r.Context()
		payload, err := i.validate(ctx, value, i.Audience)
		if err != nil {
			// Verifier errors may contain values from the supplied credential.
			http.Error(w, "Invalid Google ID token", http.StatusUnauthorized)
			return
		}
		email := googleVerifiedEmail(payload)
		if email == "" {
			http.Error(w, "Invalid Google ID token", http.StatusUnauthorized)
			return
		}

		user, err := i.Users.FindByEmail(ctx, email)
		if err != nil {
			errutil.Handle(ctx, goerr.Wrap(err, "Slack user directory lookup failed"), "Failed to resolve MCP Slack identity")
			http.Error(w, "Slack identity lookup unavailable", http.StatusServiceUnavailable)
			return
		}
		if user == nil || user.ID == "" {
			http.Error(w, "Slack identity not authorized", http.StatusForbidden)
			return
		}

		ctx = auth.ContextWithToken(ctx, &auth.Token{Sub: string(user.ID)})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// googleVerifiedEmail checks claims beyond the Google library's signature,
// audience and expiry checks. A verified external Google Account email without
// a hosted domain may no longer be controlled by the account holder.
func googleVerifiedEmail(payload *idtoken.Payload) string {
	if payload == nil || (payload.Issuer != "https://accounts.google.com" && payload.Issuer != "accounts.google.com") || payload.Subject == "" {
		return ""
	}
	email, _ := payload.Claims["email"].(string)
	verified, _ := payload.Claims["email_verified"].(bool)
	hostedDomain, _ := payload.Claims["hd"].(string)
	if !verified || (hostedDomain == "" && !strings.HasSuffix(strings.ToLower(email), "@gmail.com")) {
		return ""
	}
	return email
}
