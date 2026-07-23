package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/ai"
	"github.com/apex42group/ledgermancy/backend/internal/alerts"
	"github.com/apex42group/ledgermancy/backend/internal/auth"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// defaultEventLimit caps how many events the list endpoint returns; the bell
// only needs the most recent, and older ones age out of interest.
const defaultEventLimit = 100

type alertResponse struct {
	ID      uuid.UUID       `json:"id"`
	Type    string          `json:"type"`
	Config  json.RawMessage `json:"config"`
	Enabled bool            `json:"enabled"`
}

// toAlertResponse converts the stored []byte config into raw JSON so it reaches
// the client as an object, not a base64 string (which is how Go marshals a
// []byte field).
func toAlertResponse(a dbgen.Alert) alertResponse {
	return alertResponse{
		ID:      a.ID,
		Type:    a.Type,
		Config:  json.RawMessage(a.Config),
		Enabled: a.Enabled,
	}
}

func (s *Server) handleListAlerts(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListAlerts(r.Context(), identity.HouseholdID)
	if err != nil {
		s.internalError(w, "list alerts", err)
		return
	}

	out := make([]alertResponse, 0, len(rows))
	for _, a := range rows {
		out = append(out, toAlertResponse(a))
	}
	writeJSON(w, http.StatusOK, out)
}

type upsertAlertRequest struct {
	Type    string          `json:"type"`
	Config  json.RawMessage `json:"config"`
	Enabled *bool           `json:"enabled"`
}

// validateAlertBody checks the type and config, returning the config bytes to
// store. enabled defaults to true.
func validateAlertBody(req upsertAlertRequest) (config []byte, enabled bool, err error) {
	if !alerts.IsValidType(req.Type) {
		return nil, false, errors.New("unknown alert type")
	}
	if len(req.Config) == 0 {
		return nil, false, errors.New("config is required")
	}
	if err := alerts.ValidateConfig(req.Type, req.Config); err != nil {
		return nil, false, err
	}
	enabled = true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	return req.Config, enabled, nil
}

func (s *Server) handleCreateAlert(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req upsertAlertRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	config, enabled, err := validateAlertBody(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	alert, err := s.Queries.CreateAlert(r.Context(), dbgen.CreateAlertParams{
		HouseholdID: identity.HouseholdID,
		Type:        req.Type,
		Config:      config,
		Enabled:     enabled,
	})
	if err != nil {
		s.internalError(w, "create alert", err)
		return
	}

	// Evaluate immediately so a newly-enabled alert surfaces without waiting for
	// the sweep.
	s.enqueueAlertEval(identity.HouseholdID)
	writeJSON(w, http.StatusCreated, toAlertResponse(alert))
}

func (s *Server) handleUpdateAlert(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	alertID, err := uuid.Parse(chi.URLParam(r, "alertID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid alert id")
		return
	}

	var req upsertAlertRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// The type of an existing alert is fixed; take it from the stored row so the
	// caller cannot repurpose a row into a different, mis-configured type.
	existing, err := s.Queries.GetAlert(r.Context(), dbgen.GetAlertParams{
		ID: alertID, HouseholdID: identity.HouseholdID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "alert not found")
		return
	}
	if err != nil {
		s.internalError(w, "get alert", err)
		return
	}
	req.Type = existing.Type

	config, enabled, err := validateAlertBody(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	alert, err := s.Queries.UpdateAlert(r.Context(), dbgen.UpdateAlertParams{
		ID: alertID, HouseholdID: identity.HouseholdID, Config: config, Enabled: enabled,
	})
	if err != nil {
		s.internalError(w, "update alert", err)
		return
	}

	s.enqueueAlertEval(identity.HouseholdID)
	writeJSON(w, http.StatusOK, toAlertResponse(alert))
}

func (s *Server) handleDeleteAlert(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	alertID, err := uuid.Parse(chi.URLParam(r, "alertID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid alert id")
		return
	}

	if err := s.Queries.DeleteAlert(r.Context(), dbgen.DeleteAlertParams{
		ID: alertID, HouseholdID: identity.HouseholdID,
	}); err != nil {
		s.internalError(w, "delete alert", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --------------------------------------------------------------------------
// Natural-language rule parsing
//
// The model does ONE thing: NL → a structured proposal drawn from the four
// existing alert types or a budget. It is never trusted to have produced a valid
// config: every parsed alert is run through alerts.ValidateConfig, every budget
// category is resolved against the real household list, and every amount is
// re-parsed as a positive decimal — here, deterministically. A parse that fails
// any of these becomes an honest "unsupported" proposal, never a saved rule. No
// writing happens on parse; confirmation calls the existing CRUD endpoints. A
// bad parse is an expected outcome, so it returns 200 with kind=unsupported,
// never a 500.
// --------------------------------------------------------------------------

type parseAlertRequest struct {
	Text string `json:"text"`
}

type parsedAlertProposal struct {
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config"`
}

type parsedBudgetProposal struct {
	CategoryID   uuid.UUID `json:"category_id"`
	CategorySlug string    `json:"category_slug"`
	CategoryName string    `json:"category_name"`
	Amount       string    `json:"amount"`
}

type parseRuleResponse struct {
	Kind    string                `json:"kind"` // "alert" | "budget" | "unsupported"
	Alert   *parsedAlertProposal  `json:"alert,omitempty"`
	Budget  *parsedBudgetProposal `json:"budget,omitempty"`
	Summary string                `json:"summary,omitempty"`
	Caveats []string              `json:"caveats,omitempty"`
	Reason  string                `json:"reason,omitempty"`
}

func (s *Server) handleParseAlert(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	if !s.AI.Enabled() {
		writeError(w, http.StatusServiceUnavailable, "AI features are not configured")
		return
	}

	var req parseAlertRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}

	cats, err := s.Queries.ListCategories(r.Context(), &identity.HouseholdID)
	if err != nil {
		s.internalError(w, "list categories for parse", err)
		return
	}
	refs := make([]ai.CategoryRef, 0, len(cats))
	for _, c := range cats {
		if c.IsIncome || c.IsTransfer {
			continue // budgets are for spending categories only
		}
		refs = append(refs, ai.CategoryRef{Name: c.Name, Slug: c.Slug})
	}

	today := time.Now().Format("Monday, 2 January 2006")
	parsed, err := s.AI.ParseRule(r.Context(), req.Text, refs, today)
	if err != nil {
		s.internalError(w, "parse rule", err)
		return
	}

	writeJSON(w, http.StatusOK, resolveRuleProposal(parsed, cats))
}

// ruleConfigView reads any alert config's fields with money as decimal, so it
// tolerates the model emitting an amount as a JSON string or number.
type ruleConfigView struct {
	Threshold  decimal.Decimal `json:"threshold"`
	Percent    int             `json:"percent"`
	RecentDays int             `json:"recent_days"`
	MinAmount  decimal.Decimal `json:"min_amount"`
	Floor      decimal.Decimal `json:"floor"`
}

// resolveRuleProposal turns a model parse into a confirmable proposal, applying
// every deterministic gate. It never returns an error: an unenforceable or
// unresolvable parse becomes kind=unsupported. Money in the returned config is
// re-emitted canonically (fixed-2 strings) so the confirm/edit UI and the write
// path see clean values. The summary describes engine reality, not the user's
// phrasing.
func resolveRuleProposal(parsed ai.ParsedRule, cats []dbgen.Category) parseRuleResponse {
	switch parsed.Kind {
	case "alert":
		if !alerts.IsValidType(parsed.AlertType) {
			return unsupported("That doesn't map to an alert this app can enforce yet.")
		}
		if err := alerts.ValidateConfig(parsed.AlertType, parsed.Config); err != nil {
			return unsupported("Couldn't turn that into a valid alert rule.")
		}
		var view ruleConfigView
		_ = json.Unmarshal(parsed.Config, &view)
		config, summary := canonicalAlert(parsed.AlertType, view)

		resp := parseRuleResponse{
			Kind:    "alert",
			Alert:   &parsedAlertProposal{Type: parsed.AlertType, Config: config},
			Summary: summary,
		}
		if c := strings.TrimSpace(parsed.Reason); c != "" {
			resp.Caveats = []string{c}
		}
		return resp

	case "budget":
		cat, ok := resolveCategory(parsed.Category, cats)
		if !ok {
			return unsupported("Couldn't match that to one of your spending categories.")
		}
		amount, err := decimal.NewFromString(strings.TrimSpace(parsed.Amount))
		if err != nil || !amount.IsPositive() {
			return unsupported("Couldn't read a budget amount from that.")
		}
		return parseRuleResponse{
			Kind: "budget",
			Budget: &parsedBudgetProposal{
				CategoryID:   cat.ID,
				CategorySlug: cat.Slug,
				CategoryName: cat.Name,
				Amount:       amount.StringFixed(2),
			},
			Summary: fmt.Sprintf("Budget $%s per month for %s.", amount.StringFixed(2), cat.Name),
		}

	default:
		reason := strings.TrimSpace(parsed.Reason)
		if reason == "" {
			reason = "That isn't something this app can set up yet."
		}
		return unsupported(reason)
	}
}

func unsupported(reason string) parseRuleResponse {
	return parseRuleResponse{Kind: "unsupported", Reason: reason}
}

// canonicalAlert re-emits a validated config with fixed-2 money and applied
// defaults, and returns the deterministic human summary of what will actually be
// enforced. The look-back windows quoted match the engine constants.
func canonicalAlert(alertType string, v ruleConfigView) (json.RawMessage, string) {
	switch alertType {
	case alerts.TypeBigSpend:
		cfg, _ := json.Marshal(map[string]string{"threshold": v.Threshold.StringFixed(2)})
		return cfg, fmt.Sprintf(
			"Flag any single purchase over $%s (checked over the last 30 days).",
			v.Threshold.StringFixed(2))

	case alerts.TypeBudgetThreshold:
		cfg, _ := json.Marshal(map[string]int{"percent": v.Percent})
		return cfg, fmt.Sprintf(
			"Warn when a category's spending reaches %d%% of its monthly budget.", v.Percent)

	case alerts.TypeUnusualMerchant:
		days := v.RecentDays
		if days <= 0 {
			days = 7 // engine default
		}
		cfg, _ := json.Marshal(map[string]any{
			"recent_days": days,
			"min_amount":  v.MinAmount.StringFixed(2),
		})
		return cfg, fmt.Sprintf(
			"Flag a merchant that first appears within the last %d days, on a charge of at least $%s.",
			days, v.MinAmount.StringFixed(2))

	case alerts.TypeLowLeftover:
		cfg, _ := json.Marshal(map[string]string{"floor": v.Floor.StringFixed(2)})
		return cfg, fmt.Sprintf(
			"Warn when money left this month (income minus spending) drops below $%s.",
			v.Floor.StringFixed(2))

	default:
		return json.RawMessage("{}"), ""
	}
}

// resolveCategory matches a parsed name-or-slug against the household's spending
// categories, case-insensitively. Income/transfer categories are never budget
// targets.
func resolveCategory(nameOrSlug string, cats []dbgen.Category) (dbgen.Category, bool) {
	want := strings.ToLower(strings.TrimSpace(nameOrSlug))
	if want == "" {
		return dbgen.Category{}, false
	}
	for _, c := range cats {
		if c.IsIncome || c.IsTransfer {
			continue
		}
		if strings.ToLower(c.Slug) == want || strings.ToLower(c.Name) == want {
			return c, true
		}
	}
	return dbgen.Category{}, false
}

type alertEventResponse struct {
	ID          uuid.UUID       `json:"id"`
	AlertType   string          `json:"alert_type"`
	Payload     json.RawMessage `json:"payload"`
	TriggeredAt string          `json:"triggered_at"`
	Read        bool            `json:"read"`
}

func (s *Server) handleListAlertEvents(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListAlertEvents(r.Context(), dbgen.ListAlertEventsParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Limit:       defaultEventLimit,
	})
	if err != nil {
		s.internalError(w, "list alert events", err)
		return
	}

	out := make([]alertEventResponse, 0, len(rows))
	for _, e := range rows {
		out = append(out, alertEventResponse{
			ID:          e.ID,
			AlertType:   e.AlertType,
			Payload:     json.RawMessage(e.Payload),
			TriggeredAt: e.TriggeredAt.UTC().Format("2006-01-02T15:04:05Z"),
			Read:        e.ReadAt != nil,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleUnreadAlertCount(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	count, err := s.Queries.CountUnreadAlertEvents(r.Context(), dbgen.CountUnreadAlertEventsParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
	})
	if err != nil {
		s.internalError(w, "count unread alerts", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"count": count})
}

func (s *Server) handleMarkAlertEventRead(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	eventID, err := uuid.Parse(chi.URLParam(r, "eventID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid event id")
		return
	}

	if err := s.Queries.MarkAlertEventRead(r.Context(), dbgen.MarkAlertEventReadParams{
		ID: eventID, HouseholdID: identity.HouseholdID, UserID: identity.UserID,
	}); err != nil {
		s.internalError(w, "mark alert read", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMarkAllAlertEventsRead(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	if err := s.Queries.MarkAllAlertEventsRead(r.Context(), dbgen.MarkAllAlertEventsReadParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID,
	}); err != nil {
		s.internalError(w, "mark all alerts read", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
