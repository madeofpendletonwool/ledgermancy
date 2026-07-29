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

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
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
	// Role is the login's permission level: owner | member | child. It is
	// about what this login may do, not about who the person is.
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
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
			Role:        row.Role,
			CreatedAt:   row.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, members)
}

type createInviteRequest struct {
	Email string `json:"email"`
	// Role the invite grants. Empty means 'member', which is what an invite
	// meant before roles existed.
	Role string `json:"role"`
	// PersonID attaches the new login to an EXISTING person instead of
	// creating a second one. This is how "enable a login for Ellie" works;
	// without it, accepting the invite would produce a duplicate Ellie with
	// none of the first one's accounts, goals or allowance attached.
	PersonID *uuid.UUID `json:"person_id"`
}

type createInviteResponse struct {
	ID        uuid.UUID  `json:"id"`
	Email     string     `json:"email"`
	Role      string     `json:"role"`
	PersonID  *uuid.UUID `json:"person_id"`
	ExpiresAt time.Time  `json:"expires_at"`
	// Token is returned exactly once, at creation. Only its hash is stored,
	// so it cannot be recovered later — the inviter must pass it along now.
	Token string `json:"token"`
}

func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()

	var req createInviteRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	// A real, deliverable address is required even for a child's login.
	//
	// The tempting alternative — letting a parent invent `ellie@household.local`
	// so they can just set a password — would break password reset and every
	// security notification silently, and the failure surfaces at the worst
	// possible moment. A parent giving a young child a login uses a
	// plus-address they control (parent+ellie@gmail.com), which is a real
	// mailbox. The UI says so rather than leaving them to find out.
	if _, err := mail.ParseAddress(req.Email); err != nil {
		writeError(w, http.StatusBadRequest, "a valid email address is required")
		return
	}

	role := req.Role
	if role == "" {
		role = auth.RoleMember
	}
	switch role {
	case auth.RoleMember, auth.RoleChild:
	case auth.RoleOwner:
		// A household has exactly one owner and it is not granted by invite.
		writeError(w, http.StatusBadRequest,
			"an invite cannot grant ownership; transfer it after they join")
		return
	default:
		writeError(w, http.StatusBadRequest, "role must be one of: member, child")
		return
	}

	// Resolve the person through the household guard, and refuse one that
	// already has a login — otherwise accepting this invite would silently
	// fail to link and leave a second, detached person behind.
	if req.PersonID != nil {
		person, err := s.Queries.GetPerson(ctx, dbgen.GetPersonParams{
			ID: *req.PersonID, HouseholdID: identity.HouseholdID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "person not found")
			return
		}
		if err != nil {
			s.internalError(w, "get person for invite", err)
			return
		}
		if person.UserID != nil {
			writeError(w, http.StatusConflict, "that person already has a login")
			return
		}
	}

	token, err := auth.NewToken()
	if err != nil {
		s.internalError(w, "generate invite token", err)
		return
	}

	invite, err := s.Queries.CreateInvite(ctx, dbgen.CreateInviteParams{
		HouseholdID: identity.HouseholdID,
		Email:       req.Email,
		TokenHash:   auth.HashToken(s.Config.SessionSecret, token),
		InvitedBy:   &identity.UserID,
		ExpiresAt:   time.Now().Add(inviteTTL),
		Role:        role,
		PersonID:    req.PersonID,
	})
	if err != nil {
		s.internalError(w, "create invite", err)
		return
	}

	writeJSON(w, http.StatusCreated, createInviteResponse{
		ID:        invite.ID,
		Email:     invite.Email,
		Role:      invite.Role,
		PersonID:  invite.PersonID,
		ExpiresAt: invite.ExpiresAt,
		Token:     token,
	})
}

type inviteResponse struct {
	ID         uuid.UUID  `json:"id"`
	Email      string     `json:"email"`
	Role       string     `json:"role"`
	PersonID   *uuid.UUID `json:"person_id"`
	PersonName *string    `json:"person_name"`
	ExpiresAt  time.Time  `json:"expires_at"`
	CreatedAt  time.Time  `json:"created_at"`
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
			ID:         row.ID,
			Email:      row.Email,
			Role:       row.Role,
			PersonID:   row.PersonID,
			PersonName: row.PersonName,
			ExpiresAt:  row.ExpiresAt,
			CreatedAt:  row.CreatedAt,
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
