package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// People: everyone the household's money can be about, whether or not they can
// sign in. A person is not a login — see the 00034 migration header.

// personResponse carries the person plus, when one exists, the login attached
// to it. Email and Role are nil for a person with no login, which is the normal
// state for a young child rather than an error.
type personResponse struct {
	ID          uuid.UUID `json:"id"`
	DisplayName string    `json:"display_name"`
	// Birthdate is date-only (YYYY-MM-DD). Nil means not set, and every
	// consumer must handle that rather than defaulting — a defaulted birthdate
	// produces a confidently wrong projection.
	Birthdate   *string    `json:"birthdate"`
	Age         *int       `json:"age"`
	IsDependent bool       `json:"is_dependent"`
	UserID      *uuid.UUID `json:"user_id"`
	Email       *string    `json:"email"`
	Role        *string    `json:"role"`
	HasLogin    bool       `json:"has_login"`
	CreatedAt   string     `json:"created_at"`
}

// AgeAt returns the age in whole years on the given day, or nil if no birthdate
// is set.
//
// Deliberately takes `now` rather than reading the clock: every projection in
// this codebase is parameterised on a `now` for the same reason (doc 15's
// ProjectRetirement), and an age derived from the wall clock would make every
// test that touches it calendar-dependent.
//
// The subtraction is the ordinary one — decrement if this year's birthday has
// not happened yet — which puts the increment exactly ON the birthday.
func AgeAt(birthdate *time.Time, now time.Time) *int {
	if birthdate == nil {
		return nil
	}
	b := *birthdate
	years := now.Year() - b.Year()
	// Compare month/day only. Using YearDay would be wrong across a leap year.
	if now.Month() < b.Month() ||
		(now.Month() == b.Month() && now.Day() < b.Day()) {
		years--
	}
	if years < 0 {
		return nil
	}
	return &years
}

func personFromRow(id uuid.UUID, name string, birthdate *time.Time, dependent bool,
	userID *uuid.UUID, email *string, role *string, createdAt time.Time,
	now time.Time,
) personResponse {
	resp := personResponse{
		ID:          id,
		DisplayName: name,
		Age:         AgeAt(birthdate, now),
		IsDependent: dependent,
		UserID:      userID,
		Email:       email,
		Role:        role,
		HasLogin:    userID != nil,
		CreatedAt:   createdAt.UTC().Format(time.RFC3339),
	}
	if birthdate != nil {
		d := birthdate.Format(time.DateOnly)
		resp.Birthdate = &d
	}
	return resp
}

func (s *Server) handleListPeople(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	now := time.Now()

	rows, err := s.Queries.ListPeople(r.Context(), identity.HouseholdID)
	if err != nil {
		s.internalError(w, "list people", err)
		return
	}

	out := make([]personResponse, 0, len(rows))
	for _, p := range rows {
		out = append(out, personFromRow(p.ID, p.DisplayName, p.Birthdate,
			p.IsDependent, p.UserID, p.Email, p.Role, p.CreatedAt, now))
	}
	writeJSON(w, http.StatusOK, out)
}

type personRequest struct {
	DisplayName string  `json:"display_name"`
	Birthdate   *string `json:"birthdate"`
	IsDependent bool    `json:"is_dependent"`
}

// parseBirthdate accepts a YYYY-MM-DD string or nil. A birthdate in the future
// is refused: it is always a typo, and it would produce a negative age that
// every consumer downstream would have to defend against.
func parseBirthdate(raw *string, now time.Time) (*time.Time, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, nil
	}
	d, err := time.Parse(time.DateOnly, strings.TrimSpace(*raw))
	if err != nil {
		return nil, errors.New("birthdate must be YYYY-MM-DD")
	}
	if d.After(now) {
		return nil, errors.New("birthdate cannot be in the future")
	}
	return &d, nil
}

func (s *Server) handleCreatePerson(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	now := time.Now()

	var req personRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	if req.DisplayName == "" {
		writeError(w, http.StatusBadRequest, "display_name is required")
		return
	}

	birthdate, err := parseBirthdate(req.Birthdate, now)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// UserID is nil: a person is created without a login. Attaching one is a
	// separate, deliberate step through the invite flow.
	p, err := s.Queries.CreatePerson(r.Context(), dbgen.CreatePersonParams{
		HouseholdID: identity.HouseholdID,
		UserID:      nil,
		DisplayName: req.DisplayName,
		Birthdate:   birthdate,
		IsDependent: req.IsDependent,
	})
	if err != nil {
		s.internalError(w, "create person", err)
		return
	}

	writeJSON(w, http.StatusCreated, personFromRow(p.ID, p.DisplayName, p.Birthdate,
		p.IsDependent, p.UserID, nil, nil, p.CreatedAt, now))
}

// handleUpdatePerson edits a person in the caller's household.
//
// A child may edit only their own row (their name and birthdate), never
// anyone else's, and never the is_dependent flag — that is a household
// classification, not a self-description.
func (s *Server) handleUpdatePerson(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	personID, err := uuid.Parse(chi.URLParam(r, "personID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid person id")
		return
	}
	s.updatePerson(w, r, identity, personID)
}

// handleGetMyPerson serves the caller's own person row. Available to every
// login including a child, so the client never has to know its own person id.
func (s *Server) handleGetMyPerson(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	now := time.Now()

	p, err := s.Queries.GetPersonByUserID(r.Context(), &identity.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "no person record for this login")
		return
	}
	if err != nil {
		s.internalError(w, "get my person", err)
		return
	}

	role := identity.Role
	email := identity.Email
	writeJSON(w, http.StatusOK, personFromRow(p.ID, p.DisplayName, p.Birthdate,
		p.IsDependent, p.UserID, &email, &role, p.CreatedAt, now))
}

// handleUpdateMyPerson is the self-service path — "set my own birthdate" from
// Settings, without needing the Household admin screen or knowing a person id.
// It is the same write as handleUpdatePerson against the caller's own row.
func (s *Server) handleUpdateMyPerson(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	p, err := s.Queries.GetPersonByUserID(r.Context(), &identity.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "no person record for this login")
		return
	}
	if err != nil {
		s.internalError(w, "get my person", err)
		return
	}
	s.updatePerson(w, r, identity, p.ID)
}

// updatePerson is the shared body of the two update paths above.
func (s *Server) updatePerson(w http.ResponseWriter, r *http.Request,
	identity auth.Identity, personID uuid.UUID,
) {
	ctx := r.Context()
	now := time.Now()

	existing, err := s.Queries.GetPerson(ctx, dbgen.GetPersonParams{
		ID: personID, HouseholdID: identity.HouseholdID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "person not found")
		return
	}
	if err != nil {
		s.internalError(w, "get person", err)
		return
	}

	// You may always edit yourself. Editing anybody else requires being an
	// adult — a child login must not be able to rename a sibling or change a
	// parent's birthdate.
	isSelf := existing.UserID != nil && *existing.UserID == identity.UserID
	if !isSelf && !identity.IsAdult() {
		writeError(w, http.StatusForbidden, "not permitted for this account")
		return
	}

	var req personRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	if req.DisplayName == "" {
		writeError(w, http.StatusBadRequest, "display_name is required")
		return
	}

	birthdate, err := parseBirthdate(req.Birthdate, now)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// is_dependent is a household classification. A child editing their own row
	// keeps whatever the household set rather than being able to declare
	// themselves independent.
	isDependent := req.IsDependent
	if !identity.IsAdult() {
		isDependent = existing.IsDependent
	}

	p, err := s.Queries.UpdatePerson(ctx, dbgen.UpdatePersonParams{
		ID:          personID,
		HouseholdID: identity.HouseholdID,
		DisplayName: req.DisplayName,
		Birthdate:   birthdate,
		IsDependent: isDependent,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "person not found")
		return
	}
	if err != nil {
		s.internalError(w, "update person", err)
		return
	}

	writeJSON(w, http.StatusOK, personFromRow(p.ID, p.DisplayName, p.Birthdate,
		p.IsDependent, p.UserID, existing.Email, existing.Role, p.CreatedAt, now))
}

// handleDeletePerson removes a person who has no login.
//
// A person with a login is refused rather than cascaded: deleting one takes
// their allowance history, goal contributions and splits with it, and doing
// that implicitly to somebody who can still sign in is not a decision this
// endpoint should make on the caller's behalf.
func (s *Server) handleDeletePerson(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	personID, err := uuid.Parse(chi.URLParam(r, "personID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid person id")
		return
	}

	existing, err := s.Queries.GetPerson(r.Context(), dbgen.GetPersonParams{
		ID: personID, HouseholdID: identity.HouseholdID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "person not found")
		return
	}
	if err != nil {
		s.internalError(w, "get person", err)
		return
	}
	if existing.UserID != nil {
		writeError(w, http.StatusConflict,
			"this person has a login; remove the login before deleting them")
		return
	}

	rows, err := s.Queries.DeletePerson(r.Context(), dbgen.DeletePersonParams{
		ID: personID, HouseholdID: identity.HouseholdID,
	})
	if err != nil {
		s.internalError(w, "delete person", err)
		return
	}
	if rows == 0 {
		writeError(w, http.StatusNotFound, "person not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --------------------------------------------------------------------------
// Roles
// --------------------------------------------------------------------------

type setRoleRequest struct {
	Role string `json:"role"`
}

// handleSetMemberRole changes a login's permission level. Owner-only, enforced
// by RequireOwner on the route.
func (s *Server) handleSetMemberRole(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()

	userID, err := uuid.Parse(chi.URLParam(r, "userID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}

	var req setRoleRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	switch req.Role {
	case auth.RoleOwner, auth.RoleMember, auth.RoleChild:
	default:
		writeError(w, http.StatusBadRequest, "role must be one of: owner, member, child")
		return
	}

	// Demoting the last adult would leave a household nobody can administer —
	// no invites, no institution linking, no way back without database access.
	if userID == identity.UserID && req.Role == auth.RoleChild {
		writeError(w, http.StatusBadRequest,
			"you cannot make your own account a child account")
		return
	}
	if req.Role == auth.RoleChild {
		adults, err := s.Queries.CountAdultsInHousehold(ctx, identity.HouseholdID)
		if err != nil {
			s.internalError(w, "count adults", err)
			return
		}
		current, err := s.Queries.GetUserRole(ctx, userID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			s.internalError(w, "get user role", err)
			return
		}
		// Only a demotion from adult reduces the count.
		if adults <= 1 && (current == auth.RoleOwner || current == auth.RoleMember) {
			writeError(w, http.StatusBadRequest,
				"a household needs at least one adult account")
			return
		}
	}

	row, err := s.Queries.SetUserRole(ctx, dbgen.SetUserRoleParams{
		ID: userID, HouseholdID: identity.HouseholdID, Role: req.Role,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "member not found")
		return
	}
	if err != nil {
		s.internalError(w, "set member role", err)
		return
	}

	writeJSON(w, http.StatusOK, memberResponse{
		ID: row.ID, Email: row.Email, DisplayName: row.DisplayName, Role: row.Role,
	})
}
