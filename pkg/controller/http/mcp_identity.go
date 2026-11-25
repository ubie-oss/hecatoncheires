package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/auth"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/errutil"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/safe"
	"golang.org/x/net/http/httpguts"
	"google.golang.org/api/idtoken"
	"google.golang.org/api/option"
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
	validator *idtoken.Validator
}

// WithMCPIdentity requires a verified Slack identity for all MCP requests.
func WithMCPIdentity(ctx context.Context, config MCPIdentity) (MCPOption, error) {
	return newMCPIdentityOption(ctx, config, http.DefaultTransport)
}

func newMCPIdentityOption(ctx context.Context, config MCPIdentity, transport http.RoundTripper) (MCPOption, error) {
	if !httpguts.ValidHeaderFieldName(config.Header) || strings.TrimSpace(config.Audience) == "" || config.Users == nil || config.WorkspaceAccess == nil {
		return nil, goerr.New("MCP identity requires a valid header, audience, user directory, and workspace authorizer")
	}
	validator, err := idtoken.NewValidator(ctx, option.WithHTTPClient(&http.Client{
		Transport: googleKeyTransport{base: transport},
		Timeout:   10 * time.Second,
	}))
	if err != nil {
		return nil, goerr.Wrap(err, "initialize Google ID token validator")
	}
	return func(h *mcpHandler) {
		h.identity = &mcpIdentity{MCPIdentity: config, validator: validator}
	}, nil
}

var errGoogleSigningKeysUnavailable = goerr.New("Google signing keys unavailable")

// googleKeyTransport tags key retrieval failures at the source. The Google
// verifier also returns parse and signature errors, which still mean 401.
type googleKeyTransport struct{ base http.RoundTripper }

func (t googleKeyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, goerr.Wrap(errGoogleSigningKeysUnavailable, "Google signing key request failed")
	}
	defer safe.Close(req.Context(), resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, goerr.Wrap(errGoogleSigningKeysUnavailable, "Google signing key endpoint failed", goerr.V("status", resp.StatusCode))
	}
	const maxKeyResponse = 64 << 10
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxKeyResponse+1))
	var set struct{ Keys []json.RawMessage }
	if err != nil || len(body) > maxKeyResponse || json.Unmarshal(body, &set) != nil || len(set.Keys) == 0 {
		return nil, goerr.Wrap(errGoogleSigningKeysUnavailable, "invalid Google signing key response")
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
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
		payload, err := i.validator.Validate(ctx, value, i.Audience)
		if err != nil {
			if errors.Is(err, errGoogleSigningKeysUnavailable) {
				if ctx.Err() == nil {
					errutil.Handle(ctx, goerr.Wrap(err, "failed to retrieve verifier keys"), "MCP token verification unavailable")
				}
				http.Error(w, "Google ID token verification unavailable", http.StatusServiceUnavailable)
				return
			}
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
