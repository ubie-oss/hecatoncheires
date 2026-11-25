package http

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/frontend"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/errutil"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/logging"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/safe"
)

type Server struct {
	router                  *chi.Mux
	enableGraphiQL          bool
	authUC                  AuthUseCase
	slackService            slack.Service
	slackWebhookHandler     *SlackWebhookHandler
	slackInteractionHandler *SlackInteractionHandler
	slackCommandHandler     *SlackCommandHandler
	slackSigningSecret      string
	workspaceRegistry       *model.WorkspaceRegistry
	workspaceAccess         interfaces.WorkspaceAuthorizer
	tickHookHandler         *TickHookHandler
	dbCheckHandler          *DBCheckHandler
	mcpHandler              http.Handler
}

type Options func(*Server)

func WithWorkspaceRegistry(registry *model.WorkspaceRegistry) Options {
	return func(s *Server) {
		s.workspaceRegistry = registry
	}
}

// WithWorkspaceAccess sets the authorizer /api/workspaces filters its list
// through. Without it the list is not filtered, which is correct only for a
// deployment where no workspace has an [authz] policy.
func WithWorkspaceAccess(access interfaces.WorkspaceAuthorizer) Options {
	return func(s *Server) {
		s.workspaceAccess = access
	}
}

func WithGraphiQL(enabled bool) Options {
	return func(s *Server) {
		s.enableGraphiQL = enabled
	}
}

func WithAuth(authUC AuthUseCase) Options {
	return func(s *Server) {
		s.authUC = authUC
	}
}

func WithSlackWebhook(handler *SlackWebhookHandler, signingSecret string) Options {
	return func(s *Server) {
		s.slackWebhookHandler = handler
		s.slackSigningSecret = signingSecret
	}
}

func WithSlackInteraction(handler *SlackInteractionHandler) Options {
	return func(s *Server) {
		s.slackInteractionHandler = handler
	}
}

func WithSlackService(svc slack.Service) Options {
	return func(s *Server) {
		s.slackService = svc
	}
}

func WithSlackCommand(handler *SlackCommandHandler) Options {
	return func(s *Server) {
		s.slackCommandHandler = handler
	}
}

// WithTickHook wires the POST /hooks/tick handler so external schedulers
// (Cloud Scheduler, internal cron) can trigger a sweep over scheduled
// Jobs. nil handler leaves the route unregistered.
func WithTickHook(handler *TickHookHandler) Options {
	return func(s *Server) {
		s.tickHookHandler = handler
	}
}

// WithDBCheck wires the POST /api/validate/db endpoint so operator tooling can
// run the `validate --check-db` consistency report against workspace
// configuration it submits. nil handler leaves the route unregistered.
func WithDBCheck(handler *DBCheckHandler) Options {
	return func(s *Server) {
		s.dbCheckHandler = handler
	}
}

// WithMCP wires the MCP (Model Context Protocol) endpoint at /mcp. The handler
// already embeds its own authorization (Rego policy) and request-context
// middleware, so it is mounted as-is. nil handler leaves the route
// unregistered.
func WithMCP(handler http.Handler) Options {
	return func(s *Server) {
		s.mcpHandler = handler
	}
}

func New(gqlHandler http.Handler, opts ...Options) (*Server, error) {
	r := chi.NewRouter()

	s := &Server{
		router:         r,
		enableGraphiQL: false,
	}
	for _, opt := range opts {
		opt(s)
	}

	// Middleware. Sentry sits right after RequestID so every request gets
	// its own Sentry Hub on the context (no-op when Sentry is disabled).
	// Repanic=true means panics still bubble up to chi's Recoverer.
	r.Use(middleware.RequestID)
	r.Use(errutil.SentryHTTPMiddleware)
	r.Use(accessLogger)
	r.Use(middleware.Recoverer)

	// GraphQL endpoint (must be registered before catch-all route)
	r.Route("/graphql", func(r chi.Router) {
		// Apply auth middleware
		if s.authUC != nil {
			r.Use(authMiddleware(s.authUC))
		}
		r.Post("/", gqlHandler.ServeHTTP)
		r.Get("/", gqlHandler.ServeHTTP) // Support GET for introspection
	})

	// GraphiQL playground
	if s.enableGraphiQL {
		r.Get("/graphiql", playground.Handler("GraphQL playground", "/graphql").ServeHTTP)
	}

	// Auth endpoints (if auth is configured)
	if s.authUC != nil {
		r.Route("/api/auth", func(r chi.Router) {
			r.Get("/login", authLoginHandler(s.authUC))
			r.Get("/callback", authCallbackHandler(s.authUC))
			r.Post("/logout", authLogoutHandler(s.authUC))
			r.Get("/me", authMeHandler(s.authUC))
			r.Get("/user-info", authUserInfoHandler(s.slackService))
		})
	}

	// Workspace list endpoint. It is authenticated because the list is filtered
	// by the caller's workspace access.
	if s.workspaceRegistry != nil {
		handler := workspacesHandler(s.workspaceRegistry, s.workspaceAccess)
		if s.authUC != nil {
			r.With(authMiddleware(s.authUC)).Get("/api/workspaces", handler)
		} else {
			r.Get("/api/workspaces", handler)
		}
	}

	// Slack webhook endpoint (if configured) - No auth required, uses signature verification
	if s.slackWebhookHandler != nil || s.slackInteractionHandler != nil || s.slackCommandHandler != nil {
		r.Route("/hooks/slack", func(r chi.Router) {
			// Apply Slack signature verification middleware to all /hooks/slack/* routes
			r.Use(SlackSignatureMiddleware(s.slackSigningSecret))

			// Event webhook endpoint
			if s.slackWebhookHandler != nil {
				r.Post("/event", s.slackWebhookHandler.ServeHTTP)
			}

			// Interaction endpoint (button clicks, modal submissions, etc.)
			if s.slackInteractionHandler != nil {
				r.Post("/interaction", s.slackInteractionHandler.ServeHTTP)
			}

			// Slash command endpoints
			if s.slackCommandHandler != nil {
				r.Post("/command", s.slackCommandHandler.ServeHTTP)
				r.Post("/command/{ws_id}", s.slackCommandHandler.ServeHTTP)
			}
		})
	}

	// Scheduled Job sweep endpoint. Unauthenticated by design (deploy
	// behind IAP / private network).
	if s.tickHookHandler != nil {
		r.Post("/hooks/tick", s.tickHookHandler.ServeHTTP)
	}

	// DB consistency check endpoint. Unauthenticated by design, same as
	// /hooks/tick — and the response names Case / Action / Memo ids, so the
	// deployment must keep it off the public internet.
	if s.dbCheckHandler != nil {
		r.Post("/api/validate/db", s.dbCheckHandler.ServeHTTP)
	}

	// MCP endpoint (Streamable HTTP). The handler embeds its own authentication
	// and authorization, so no Web auth middleware is applied here. Must be registered
	// before the catch-all SPA route.
	if s.mcpHandler != nil {
		r.Handle("/mcp", s.mcpHandler)
	}

	// Static file serving for SPA (catch-all, must be last)
	staticFS, err := fs.Sub(frontend.StaticFiles, "dist")
	if err != nil {
		return nil, goerr.Wrap(err, "failed to bind dist dir for static")
	}

	r.Get("/*", spaHandler(staticFS))

	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

// accessLogger is a middleware that logs HTTP requests
func accessLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		defer func() {
			logging.Default().Info("access",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.Status(),
				"bytes", ww.BytesWritten(),
				"duration", time.Since(start),
				"remote", r.RemoteAddr,
				"user_agent", r.UserAgent(),
			)
		}()

		next.ServeHTTP(ww, r)
	})
}

// workspacesHandler returns a handler that serves the workspace list as JSON
func workspacesHandler(registry *model.WorkspaceRegistry, access interfaces.WorkspaceAuthorizer) http.HandlerFunc {
	type workspaceResponse struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Emoji string `json:"emoji,omitempty"`
		Color string `json:"color,omitempty"`
	}
	type response struct {
		Workspaces []workspaceResponse `json:"workspaces"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		entries := registry.List()
		if access != nil {
			filtered, err := access.FilterAccessibleForCurrentUser(r.Context(), entries)
			if err != nil {
				errutil.HandleHTTP(r.Context(), w, goerr.Wrap(err, "failed to filter accessible workspaces"), http.StatusInternalServerError)
				return
			}
			entries = filtered
		}
		resp := response{
			Workspaces: make([]workspaceResponse, len(entries)),
		}
		for i, e := range entries {
			resp.Workspaces[i] = workspaceResponse{
				ID:    e.Workspace.ID,
				Name:  e.Workspace.Name,
				Emoji: e.Workspace.Emoji,
				Color: e.Workspace.Color,
			}
		}

		data, err := json.Marshal(resp)
		if err != nil {
			errutil.HandleHTTP(r.Context(), w, goerr.Wrap(err, "failed to marshal workspaces response"), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// #nosec G104 -- header already committed, write errors are unactionable
		w.Write(data) //nolint:errcheck
	}
}

// spaHandler handles SPA routing by serving static files and falling back to index.html
func spaHandler(staticFS fs.FS) http.HandlerFunc {
	fileServer := http.FileServer(http.FS(staticFS))

	return func(w http.ResponseWriter, r *http.Request) {
		urlPath := strings.TrimPrefix(r.URL.Path, "/")

		// If the path is empty, serve index.html
		if urlPath == "" {
			urlPath = "index.html"
		}

		// Try to open the file to check if it exists
		if file, err := staticFS.Open(urlPath); err != nil {
			// File not found, serve index.html for SPA routing
			if indexFile, err := staticFS.Open("index.html"); err == nil {
				defer safe.Close(r.Context(), indexFile)
				w.Header().Set("Content-Type", "text/html")
				safe.Copy(r.Context(), w, indexFile)
				return
			}

			// If index.html is also not found, return 404
			http.NotFound(w, r)
			return
		} else {
			// File exists, close it and let fileServer handle it
			safe.Close(r.Context(), file)
		}

		// Serve the requested file using the file server
		fileServer.ServeHTTP(w, r)
	}
}
