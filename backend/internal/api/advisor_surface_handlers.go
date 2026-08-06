package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/advisor"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// The advisor SURFACE (doc 31): the briefing, saved conversations, tracked
// action items, and the two household-profile fields.
//
// advisor_handlers.go next door owns the RANKER (doc 24) — the "here is what
// this month's slack would do" endpoint. This file owns everything the page is
// built around it from. Kept apart because they fail differently: the ranker is
// one pure computation with no writes, and this has four write paths that all
// have to enforce household scope.
//
// Nothing here executes anything. An action item is a note the household made
// about a decision; there is no transfer, no payment, and no column that is a
// step towards one.

// --------------------------------------------------------------------------
// Briefing
// --------------------------------------------------------------------------

type briefingResponse struct {
	AsOf string `json:"as_of"`

	NetWorth     string `json:"net_worth"`
	Assets       string `json:"assets"`
	Debts        string `json:"debts"`
	MonthlySlack string `json:"monthly_slack"`
	SlackBasis   string `json:"slack_basis"`
	IncomeMonths int    `json:"income_months"`

	FIAge               *int `json:"fi_age"`
	AlreadyFI           bool `json:"already_fi"`
	RetirementProjected bool `json:"retirement_projected"`

	DebtFree  debtFreeResponse        `json:"debt_free"`
	Runway    runwayResponse          `json:"runway"`
	Attention []attentionItemResponse `json:"attention"`
}

type debtFreeResponse struct {
	Date          *string  `json:"date"`
	Never         bool     `json:"never"`
	NeverAccount  string   `json:"never_account,omitempty"`
	Projected     int      `json:"projected"`
	Excluded      int      `json:"excluded"`
	ExcludedNames []string `json:"excluded_names"`
	TotalBalance  string   `json:"total_balance"`
}

type runwayResponse struct {
	Liquid       string  `json:"liquid"`
	MonthlyFixed string  `json:"monthly_fixed"`
	Months       *string `json:"months"`
	TargetMonths int     `json:"target_months"`
}

type attentionItemResponse struct {
	ID       uuid.UUID `json:"id"`
	Kind     string    `json:"kind"`
	Priority int       `json:"priority"`
	Title    string    `json:"title"`
	Body     string    `json:"body"`
}

// handleBriefing returns the household's opening position.
//
// NO AI ANYWHERE IN THIS PATH. With no key configured this endpoint returns
// exactly what it returns with one; the model's only role in the briefing is to
// phrase a summary in the chat, over these already-finished figures.
func (s *Server) handleBriefing(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	b, err := advisor.BuildBriefing(r.Context(), s.Queries, identity.HouseholdID, time.Now().UTC())
	if err != nil {
		s.internalError(w, "build advisor briefing", err)
		return
	}

	resp := briefingResponse{
		AsOf:                b.AsOf.Format(time.RFC3339),
		NetWorth:            advisorMoney(b.NetWorth),
		Assets:              advisorMoney(b.Assets),
		Debts:               advisorMoney(b.Debts),
		MonthlySlack:        advisorMoney(b.MonthlySlack),
		SlackBasis:          b.SlackBasis,
		IncomeMonths:        b.IncomeMonths,
		FIAge:               b.FIAge,
		AlreadyFI:           b.AlreadyFI,
		RetirementProjected: b.RetirementProjected,
		DebtFree: debtFreeResponse{
			Never:         b.DebtFree.Never,
			NeverAccount:  b.DebtFree.NeverAccount,
			Projected:     b.DebtFree.Projected,
			Excluded:      b.DebtFree.Excluded,
			ExcludedNames: b.DebtFree.ExcludedNames,
			TotalBalance:  advisorMoney(b.DebtFree.TotalBalance),
		},
		Runway: runwayResponse{
			Liquid:       advisorMoney(b.Runway.Liquid),
			MonthlyFixed: advisorMoney(b.Runway.MonthlyFixed),
			TargetMonths: b.Runway.TargetMonths,
		},
		Attention: make([]attentionItemResponse, 0, len(b.Attention)),
	}
	if b.DebtFree.Date != nil {
		d := b.DebtFree.Date.Format(time.DateOnly)
		resp.DebtFree.Date = &d
	}
	if b.Runway.Months != nil {
		// One decimal place, and a string like every other figure here: a runway
		// of 3.4 months is a measurement, and handing the browser 3.4000000001
		// to render is how a strip starts printing nonsense.
		m := b.Runway.Months.String()
		resp.Runway.Months = &m
	}
	for _, a := range b.Attention {
		resp.Attention = append(resp.Attention, attentionItemResponse{
			ID: a.ID, Kind: a.Kind, Priority: a.Priority, Title: a.Title, Body: a.Body,
		})
	}

	writeJSON(w, http.StatusOK, resp)
}

// --------------------------------------------------------------------------
// Threads
// --------------------------------------------------------------------------

type threadResponse struct {
	ID           uuid.UUID  `json:"id"`
	UserID       *uuid.UUID `json:"user_id"`
	Title        string     `json:"title"`
	IsShared     bool       `json:"is_shared"`
	MessageCount int64      `json:"message_count"`
	CreatedAt    string     `json:"created_at"`
	UpdatedAt    string     `json:"updated_at"`
}

type threadDetailResponse struct {
	threadResponse
	Messages []threadMessageResponse `json:"messages"`
}

type threadMessageResponse struct {
	ID        uuid.UUID `json:"id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	CreatedAt string    `json:"created_at"`
	// ToolTrace is the tool calls and results behind an assistant turn, opened
	// from the sealed column. It is what lets a reloaded thread re-render its
	// charts and what lets the UI grey a stale figure rather than reprinting it
	// as current. Absent on a user turn and on an assistant turn that called no
	// tools.
	ToolTrace []chatToolTrace `json:"tool_trace,omitempty"`
	// Stale marks every figure in this message as historical. Set on every
	// persisted turn, because it always is one — the reload prompt says the same
	// thing to the model, and the UI should say it to the reader.
	Stale bool `json:"stale"`
}

type createThreadRequest struct {
	Title string `json:"title"`
	// IsShared defaults to TRUE when omitted, following the account-visibility
	// convention: a household surface is shared unless somebody says otherwise.
	IsShared *bool `json:"is_shared"`
}

// maxThreadTitle bounds a title so the sidebar cannot be filled with a pasted
// essay.
const maxThreadTitle = 200

func (s *Server) handleListThreads(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	userID := identity.UserID

	rows, err := s.Queries.ListAdvisorThreads(r.Context(), dbgen.ListAdvisorThreadsParams{
		HouseholdID: identity.HouseholdID, UserID: &userID,
	})
	if err != nil {
		s.internalError(w, "list advisor threads", err)
		return
	}

	out := make([]threadResponse, 0, len(rows))
	for _, t := range rows {
		out = append(out, threadResponse{
			ID: t.ID, UserID: t.UserID, Title: t.Title, IsShared: t.IsShared,
			MessageCount: t.MessageCount,
			CreatedAt:    t.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt:    t.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateThread(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req createThreadRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = "New conversation"
	}
	if len(title) > maxThreadTitle {
		title = title[:maxThreadTitle]
	}
	shared := true
	if req.IsShared != nil {
		shared = *req.IsShared
	}

	userID := identity.UserID
	t, err := s.Queries.CreateAdvisorThread(r.Context(), dbgen.CreateAdvisorThreadParams{
		HouseholdID: identity.HouseholdID, UserID: &userID,
		Title: title, IsShared: shared,
	})
	if err != nil {
		s.internalError(w, "create advisor thread", err)
		return
	}

	writeJSON(w, http.StatusCreated, threadResponse{
		ID: t.ID, UserID: t.UserID, Title: t.Title, IsShared: t.IsShared,
		CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: t.UpdatedAt.UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleGetThread(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	userID := identity.UserID

	id, ok := parseThreadID(w, r)
	if !ok {
		return
	}

	t, err := s.Queries.GetAdvisorThread(r.Context(), dbgen.GetAdvisorThreadParams{
		ID: id, HouseholdID: identity.HouseholdID, UserID: &userID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// 404 rather than 403, and for a thread from another household as well
		// as a spouse's private one: "you may not read this" and "this does not
		// exist" are the same answer, and distinguishing them leaks which ids
		// are real.
		writeError(w, http.StatusNotFound, "conversation not found")
		return
	} else if err != nil {
		s.internalError(w, "get advisor thread", err)
		return
	}

	rows, err := s.Queries.ListAdvisorMessages(r.Context(), dbgen.ListAdvisorMessagesParams{
		ThreadID: id, HouseholdID: identity.HouseholdID, UserID: &userID,
	})
	if err != nil {
		s.internalError(w, "list advisor messages", err)
		return
	}

	resp := threadDetailResponse{
		threadResponse: threadResponse{
			ID: t.ID, UserID: t.UserID, Title: t.Title, IsShared: t.IsShared,
			MessageCount: int64(len(rows)),
			CreatedAt:    t.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt:    t.UpdatedAt.UTC().Format(time.RFC3339),
		},
		Messages: make([]threadMessageResponse, 0, len(rows)),
	}

	for _, m := range rows {
		content, err := s.Cipher.OpenString(m.Content)
		if err != nil {
			// A message that will not open is a key mismatch, and returning half
			// a transcript would be worse than saying so: the reader would have
			// no way to tell which turns were missing.
			s.internalError(w, "open advisor message", err)
			return
		}
		msg := threadMessageResponse{
			ID: m.ID, Role: m.Role, Content: content, Stale: true,
			CreatedAt: m.CreatedAt.UTC().Format(time.RFC3339),
		}
		if len(m.ToolTrace) > 0 {
			raw, err := s.Cipher.Open(m.ToolTrace)
			if err != nil {
				s.internalError(w, "open advisor tool trace", err)
				return
			}
			if err := decodeToolTrace(raw, &msg.ToolTrace); err != nil {
				s.internalError(w, "decode advisor tool trace", err)
				return
			}
		}
		resp.Messages = append(resp.Messages, msg)
	}

	writeJSON(w, http.StatusOK, resp)
}

type renameThreadRequest struct {
	Title string `json:"title"`
}

func (s *Server) handleRenameThread(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	userID := identity.UserID

	id, ok := parseThreadID(w, r)
	if !ok {
		return
	}

	var req renameThreadRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	if len(title) > maxThreadTitle {
		title = title[:maxThreadTitle]
	}

	t, err := s.Queries.RenameAdvisorThread(r.Context(), dbgen.RenameAdvisorThreadParams{
		ID: id, HouseholdID: identity.HouseholdID, UserID: &userID, Title: title,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "conversation not found")
		return
	} else if err != nil {
		s.internalError(w, "rename advisor thread", err)
		return
	}

	writeJSON(w, http.StatusOK, threadResponse{
		ID: t.ID, UserID: t.UserID, Title: t.Title, IsShared: t.IsShared,
		CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: t.UpdatedAt.UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleDeleteThread(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	userID := identity.UserID

	id, ok := parseThreadID(w, r)
	if !ok {
		return
	}

	n, err := s.Queries.DeleteAdvisorThread(r.Context(), dbgen.DeleteAdvisorThreadParams{
		ID: id, HouseholdID: identity.HouseholdID, UserID: &userID,
	})
	if err != nil {
		s.internalError(w, "delete advisor thread", err)
		return
	}
	// Zero rows means the scope check declined, not that the delete succeeded
	// vacuously. Returning 204 here would tell a caller they had removed another
	// household's conversation.
	if n == 0 {
		writeError(w, http.StatusNotFound, "conversation not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func parseThreadID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "threadID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid conversation id")
		return uuid.Nil, false
	}
	return id, true
}

// --------------------------------------------------------------------------
// Action items
// --------------------------------------------------------------------------

type actionItemResponse struct {
	ID          uuid.UUID `json:"id"`
	Title       string    `json:"title"`
	Detail      *string   `json:"detail"`
	Source      string    `json:"source"`
	Status      string    `json:"status"`
	DueDate     *string   `json:"due_date"`
	CreatedAt   string    `json:"created_at"`
	CompletedAt *string   `json:"completed_at"`
}

type createActionItemRequest struct {
	Title   string  `json:"title"`
	Detail  *string `json:"detail"`
	Source  string  `json:"source"`
	DueDate *string `json:"due_date"`
}

type updateActionItemRequest struct {
	Status string `json:"status"`
}

var (
	validActionSources  = map[string]bool{"option": true, "allocation": true, "thread": true, "manual": true}
	validActionStatuses = map[string]bool{"open": true, "done": true, "dismissed": true}
)

const maxActionTitle = 300

func (s *Server) handleListActionItems(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var status *string
	if raw := strings.TrimSpace(r.URL.Query().Get("status")); raw != "" {
		if !validActionStatuses[raw] {
			writeError(w, http.StatusBadRequest, "status must be open, done, or dismissed")
			return
		}
		status = &raw
	}

	rows, err := s.Queries.ListAdvisorActionItems(r.Context(), dbgen.ListAdvisorActionItemsParams{
		HouseholdID: identity.HouseholdID, Status: status,
	})
	if err != nil {
		s.internalError(w, "list action items", err)
		return
	}

	out := make([]actionItemResponse, 0, len(rows))
	for _, a := range rows {
		out = append(out, toActionItemResponse(a))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateActionItem(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req createActionItemRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	if len(title) > maxActionTitle {
		title = title[:maxActionTitle]
	}
	source := strings.TrimSpace(req.Source)
	if source == "" {
		source = "manual"
	}
	if !validActionSources[source] {
		writeError(w, http.StatusBadRequest, "source must be option, allocation, thread, or manual")
		return
	}

	var due *time.Time
	if req.DueDate != nil && strings.TrimSpace(*req.DueDate) != "" {
		parsed, err := time.Parse(time.DateOnly, strings.TrimSpace(*req.DueDate))
		if err != nil {
			writeError(w, http.StatusBadRequest, "due_date must be YYYY-MM-DD")
			return
		}
		due = &parsed
	}

	a, err := s.Queries.CreateAdvisorActionItem(r.Context(), dbgen.CreateAdvisorActionItemParams{
		HouseholdID: identity.HouseholdID, Title: title,
		Detail: req.Detail, Source: source, DueDate: due,
	})
	if err != nil {
		s.internalError(w, "create action item", err)
		return
	}
	writeJSON(w, http.StatusCreated, toActionItemResponse(a))
}

// handleUpdateActionItem moves an item between open, done and dismissed.
//
// STATUS ONLY. The title and detail were computed by whatever proposed the item;
// letting a tray toggle rewrite them would turn the audit trail of "what the app
// suggested and what we decided" into free text.
func (s *Server) handleUpdateActionItem(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	id, err := uuid.Parse(chi.URLParam(r, "itemID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid action item id")
		return
	}

	var req updateActionItemRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	status := strings.TrimSpace(req.Status)
	if !validActionStatuses[status] {
		writeError(w, http.StatusBadRequest, "status must be open, done, or dismissed")
		return
	}

	a, err := s.Queries.UpdateAdvisorActionItemStatus(r.Context(), dbgen.UpdateAdvisorActionItemStatusParams{
		ID: id, HouseholdID: identity.HouseholdID, Status: status,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "action item not found")
		return
	} else if err != nil {
		s.internalError(w, "update action item", err)
		return
	}
	writeJSON(w, http.StatusOK, toActionItemResponse(a))
}

func toActionItemResponse(a dbgen.AdvisorActionItem) actionItemResponse {
	out := actionItemResponse{
		ID: a.ID, Title: a.Title, Detail: a.Detail,
		Source: a.Source, Status: a.Status,
		CreatedAt: a.CreatedAt.UTC().Format(time.RFC3339),
	}
	if a.DueDate != nil {
		d := a.DueDate.Format(time.DateOnly)
		out.DueDate = &d
	}
	if a.CompletedAt != nil {
		c := a.CompletedAt.UTC().Format(time.RFC3339)
		out.CompletedAt = &c
	}
	return out
}

// --------------------------------------------------------------------------
// Household profile
// --------------------------------------------------------------------------

type householdProfileResponse struct {
	FilingStatus *string `json:"filing_status"`
	// RiskDrawdownFloor is a PERCENT: "20.00" means a 20% drawdown is the floor
	// this household is willing to plan around.
	RiskDrawdownFloor *string `json:"risk_drawdown_floor"`
}

type updateProfileRequest struct {
	// Both pointers, and both nullable on the wire: clearing a field is a real
	// operation. "I have not told you my filing status" is not "single".
	FilingStatus      *string `json:"filing_status"`
	RiskDrawdownFloor *string `json:"risk_drawdown_floor"`
}

var validFilingStatuses = map[string]bool{
	"single": true, "married_joint": true, "married_separate": true, "hoh": true,
}

func (s *Server) handleGetProfile(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	h, err := s.Queries.GetHousehold(r.Context(), identity.HouseholdID)
	if err != nil {
		s.internalError(w, "get household profile", err)
		return
	}
	writeJSON(w, http.StatusOK, toProfileResponse(h))
}

// handleUpdateProfile writes the two columns doc 31 added.
//
// Each is validated here as well as by the schema's CHECK, so a bad value comes
// back as a 400 naming the allowed set rather than as a 500 from a constraint
// violation the user cannot read.
func (s *Server) handleUpdateProfile(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req updateProfileRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var filing *string
	if req.FilingStatus != nil {
		if v := strings.TrimSpace(*req.FilingStatus); v != "" {
			if !validFilingStatuses[v] {
				writeError(w, http.StatusBadRequest,
					"filing_status must be single, married_joint, married_separate, or hoh")
				return
			}
			filing = &v
		}
	}

	var floor decimal.NullDecimal
	if req.RiskDrawdownFloor != nil {
		if v := strings.TrimSpace(*req.RiskDrawdownFloor); v != "" {
			// A string on the wire, like every other decimal in this API: a JSON
			// number would hand the browser a float to re-round.
			d, err := decimal.NewFromString(v)
			if err != nil {
				writeError(w, http.StatusBadRequest,
					"risk_drawdown_floor must be a decimal percent, e.g. \"20.00\"")
				return
			}
			if d.IsNegative() || d.GreaterThan(decimal.NewFromInt(100)) {
				writeError(w, http.StatusBadRequest,
					"risk_drawdown_floor is a percent and must be between 0 and 100")
				return
			}
			floor = decimal.NullDecimal{Decimal: d, Valid: true}
		}
	}

	h, err := s.Queries.UpdateHouseholdProfile(r.Context(), dbgen.UpdateHouseholdProfileParams{
		ID: identity.HouseholdID, FilingStatus: filing, RiskDrawdownFloor: floor,
	})
	if err != nil {
		s.internalError(w, "update household profile", err)
		return
	}
	writeJSON(w, http.StatusOK, toProfileResponse(h))
}

func toProfileResponse(h dbgen.Household) householdProfileResponse {
	out := householdProfileResponse{FilingStatus: h.FilingStatus}
	if h.RiskDrawdownFloor.Valid {
		v := h.RiskDrawdownFloor.Decimal.StringFixed(2)
		out.RiskDrawdownFloor = &v
	}
	return out
}
