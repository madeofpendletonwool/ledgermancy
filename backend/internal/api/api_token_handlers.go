package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// Personal API tokens.
//
// The surface a third-party client authenticates with. Everything security-
// relevant about it lives in internal/auth (hashing, the bearer branch in the
// middleware, the read-only scope check, the CSRF exemption); what is left here
// is the CRUD a user drives from Settings.
//
// Every route below is mounted behind auth.RequireSession, so a token cannot
// manage tokens. See that middleware for why.

// maxAPITokenNameLen bounds the one free-text field. The name is a label in a
// revoke list, not a document, and an unbounded string is an unbounded row.
const maxAPITokenNameLen = 100

// apiTokenResponse is a token as the owner sees it afterwards. There is no hash
// and no plaintext: after creation the value is genuinely gone, and this shape
// is what makes that true rather than merely intended.
type apiTokenResponse struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	LastUsedAt *time.Time `json:"last_used_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

// createdAPITokenResponse is the ONE response that carries the plaintext.
//
// It is returned by POST and never stored, logged, or readable again — the row
// holds only the HMAC. The field name is blunt on purpose: anything that ends up
// handling this struct should be obviously handling a secret.
type createdAPITokenResponse struct {
	apiTokenResponse
	Token string `json:"token"`
}

func (s *Server) handleListAPITokens(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListUserAPITokens(r.Context(), identity.UserID)
	if err != nil {
		s.internalError(w, "list api tokens", err)
		return
	}

	tokens := make([]apiTokenResponse, 0, len(rows))
	for _, row := range rows {
		tokens = append(tokens, apiTokenResponse{
			ID:         row.ID,
			Name:       row.Name,
			Scopes:     row.Scopes,
			LastUsedAt: row.LastUsedAt,
			ExpiresAt:  row.ExpiresAt,
			CreatedAt:  row.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, tokens)
}

type createAPITokenRequest struct {
	Name string `json:"name"`
	// Omitted means read-only. That default is deliberate: a client that did not
	// say it needs to write does not get to, and the UI has to ask for write
	// explicitly rather than the user discovering later that it was granted.
	Scopes []string `json:"scopes"`
	// Optional. Null or absent means the token does not expire, which is the
	// normal case for a self-hosted integration meant to keep working.
	ExpiresAt *time.Time `json:"expires_at"`
}

func (s *Server) handleCreateAPIToken(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req createAPITokenRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if len(name) > maxAPITokenNameLen {
		writeError(w, http.StatusBadRequest, "name is too long")
		return
	}

	scopes, ok := auth.NormalizeScopes(req.Scopes)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown scope; valid scopes are read and write")
		return
	}

	// An expiry already in the past would mint a token that is dead on arrival —
	// shown once, copied, and then failing every request with no explanation.
	if req.ExpiresAt != nil && !req.ExpiresAt.After(time.Now()) {
		writeError(w, http.StatusBadRequest, "expires_at must be in the future")
		return
	}

	plaintext, err := auth.NewAPIToken()
	if err != nil {
		s.internalError(w, "generate api token", err)
		return
	}

	row, err := s.Queries.CreateAPIToken(r.Context(), dbgen.CreateAPITokenParams{
		UserID:    identity.UserID,
		Name:      name,
		TokenHash: auth.HashToken(s.Config.SessionSecret, plaintext),
		Scopes:    scopes,
		ExpiresAt: req.ExpiresAt,
	})
	if err != nil {
		s.internalError(w, "create api token", err)
		return
	}

	// The name, not the token, and not a prefix of it. An audit log is read by
	// whoever can already read the database, but it is also the thing most
	// likely to be pasted into a bug report.
	s.audit(r.Context(), r, identity.UserID, "", eventAPITokenCreated,
		map[string]any{"token_id": row.ID.String(), "name": name, "scopes": scopes})

	writeJSON(w, http.StatusCreated, createdAPITokenResponse{
		apiTokenResponse: apiTokenResponse{
			ID:         row.ID,
			Name:       row.Name,
			Scopes:     row.Scopes,
			LastUsedAt: row.LastUsedAt,
			ExpiresAt:  row.ExpiresAt,
			CreatedAt:  row.CreatedAt,
		},
		Token: plaintext,
	})
}

func (s *Server) handleRevokeAPIToken(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	tokenID, err := uuid.Parse(chi.URLParam(r, "tokenID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid token id")
		return
	}

	// user_id is in the DELETE predicate, so a caller cannot revoke someone
	// else's token even with a valid id — the same shape as handleRevokeSession.
	affected, err := s.Queries.DeleteAPIToken(r.Context(), dbgen.DeleteAPITokenParams{
		ID: tokenID, UserID: identity.UserID,
	})
	if err != nil {
		s.internalError(w, "revoke api token", err)
		return
	}
	if affected == 0 {
		writeError(w, http.StatusNotFound, "token not found")
		return
	}

	// Revocation is immediate because the row is gone: authentication reads the
	// table on every request, so the next one carrying this token gets a 401.
	// Nothing caches an identity above the middleware.
	s.audit(r.Context(), r, identity.UserID, "", eventAPITokenRevoked,
		map[string]any{"token_id": tokenID.String()})
	w.WriteHeader(http.StatusNoContent)
}
