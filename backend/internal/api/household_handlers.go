package api

import (
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/apex42group/ledgermancy/backend/internal/auth"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

type householdResponse struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

func (s *Server) handleGetHousehold(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	household, err := s.Queries.GetHousehold(r.Context(), identity.HouseholdID)
	if err != nil {
		s.internalError(w, "get household", err)
		return
	}
	writeJSON(w, http.StatusOK, householdResponse{ID: household.ID, Name: household.Name})
}

type memberResponse struct {
	ID          uuid.UUID `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
}

func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	// Scoped to the caller's household, so one household can never enumerate
	// another's members.
	rows, err := s.Queries.ListHouseholdMembers(r.Context(), identity.HouseholdID)
	if err != nil {
		s.internalError(w, "list household members", err)
		return
	}

	members := make([]memberResponse, 0, len(rows))
	for _, row := range rows {
		members = append(members, memberResponse{
			ID:          row.ID,
			Email:       row.Email,
			DisplayName: row.DisplayName,
			CreatedAt:   row.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, members)
}

type createInviteRequest struct {
	Email string `json:"email"`
}

type createInviteResponse struct {
	ID        uuid.UUID `json:"id"`
	Email     string    `json:"email"`
	ExpiresAt time.Time `json:"expires_at"`
	// Token is returned exactly once, at creation. Only its hash is stored,
	// so it cannot be recovered later — the inviter must pass it along now.
	Token string `json:"token"`
}

func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req createInviteRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if _, err := mail.ParseAddress(req.Email); err != nil {
		writeError(w, http.StatusBadRequest, "a valid email address is required")
		return
	}

	token, err := auth.NewToken()
	if err != nil {
		s.internalError(w, "generate invite token", err)
		return
	}

	invite, err := s.Queries.CreateInvite(r.Context(), dbgen.CreateInviteParams{
		HouseholdID: identity.HouseholdID,
		Email:       req.Email,
		TokenHash:   auth.HashToken(s.Config.SessionSecret, token),
		InvitedBy:   &identity.UserID,
		ExpiresAt:   time.Now().Add(inviteTTL),
	})
	if err != nil {
		s.internalError(w, "create invite", err)
		return
	}

	writeJSON(w, http.StatusCreated, createInviteResponse{
		ID:        invite.ID,
		Email:     invite.Email,
		ExpiresAt: invite.ExpiresAt,
		Token:     token,
	})
}

type inviteResponse struct {
	ID        uuid.UUID `json:"id"`
	Email     string    `json:"email"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Server) handleListInvites(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListPendingInvites(r.Context(), identity.HouseholdID)
	if err != nil {
		s.internalError(w, "list invites", err)
		return
	}

	invites := make([]inviteResponse, 0, len(rows))
	for _, row := range rows {
		invites = append(invites, inviteResponse{
			ID:        row.ID,
			Email:     row.Email,
			ExpiresAt: row.ExpiresAt,
			CreatedAt: row.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, invites)
}

func (s *Server) handleDeleteInvite(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	inviteID, err := uuid.Parse(chi.URLParam(r, "inviteID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid invite id")
		return
	}

	// household_id is part of the DELETE predicate, so a caller cannot revoke
	// another household's invite even with a valid id.
	if err := s.Queries.DeleteInvite(r.Context(), dbgen.DeleteInviteParams{
		ID:          inviteID,
		HouseholdID: identity.HouseholdID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "invite not found")
			return
		}
		s.internalError(w, "delete invite", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
