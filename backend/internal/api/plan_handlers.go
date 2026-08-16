package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/advisor"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// The financial plan (MAD-258): the household's authored intent — sections of
// prose, an append-only decisions log, and a review stamp.
//
// The design doc's three rules, and where each is enforced:
//
//	LINK, DON'T COPY — the schema has no figure columns (migration 00071); a
//	section references live values by living beside them, never restating them.
//
//	APPEND, DON'T OVERWRITE — confirmed decisions are immutable; the only
//	writes storage accepts on them are INSERT and the UPDATEs below that are
//	WHERE status='proposed' by construction (plan.sql). Superseding is a new
//	row pointing back, so the history survives its own replacement.
//
//	SEALED LIKE THE TRANSCRIPTS — bodies are sealed with s.Cipher on the way in
//	and opened on the way out, exactly like advisor_messages.content.
//
// Nothing here executes anything, same posture as the advisor surface: the plan
// is what the household says it will do, and the app's job is to hold the
// sentence, not to move the money.

// --------------------------------------------------------------------------
// GET /api/plan
// --------------------------------------------------------------------------

type planSectionResponse struct {
	ID   uuid.UUID `json:"id"`
	Kind string    `json:"kind"`
	// PersonID/PersonName are set only on the 'person' kind; the CHECK on the
	// table keeps the pairing exact, so the client can branch on either.
	PersonID   *uuid.UUID `json:"person_id"`
	PersonName *string    `json:"person_name"`
	Body       string     `json:"body"`
	UpdatedAt  string     `json:"updated_at"`
}

type planDecisionResponse struct {
	ID        uuid.UUID `json:"id"`
	Topic     string    `json:"topic"`
	Body      string    `json:"body"`
	DecidedAt string    `json:"decided_at"`
	// Status is 'confirmed' (part of the log) or 'proposed' (a suggestion
	// awaiting confirmation — editable, deletable, and invisible to the
	// briefing).
	Status string `json:"status"`
	// Source says where the wording came from: typed by hand, or drafted by
	// the advisor chat and confirmed here.
	Source string `json:"source"`
	// Supersedes names the decision this one replaced; Superseded is the
	// derived "a confirmed row has replaced me" flag the log view renders.
	Supersedes *uuid.UUID `json:"supersedes"`
	Superseded bool       `json:"superseded"`
	CreatedAt  string     `json:"created_at"`
}

type financialPlanResponse struct {
	Sections  []planSectionResponse  `json:"sections"`
	Decisions []planDecisionResponse `json:"decisions"`
	// ReviewedAt is nil until somebody stamps a review. A plan that exists and
	// has never been reviewed says so honestly rather than dating itself.
	ReviewedAt *string `json:"reviewed_at"`
}

func (s *Server) handleGetFinancialPlan(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	sectionRows, err := s.Queries.ListPlanSections(r.Context(), identity.HouseholdID)
	if err != nil {
		s.internalError(w, "list plan sections", err)
		return
	}
	decisionRows, err := s.Queries.ListPlanDecisions(r.Context(), identity.HouseholdID)
	if err != nil {
		s.internalError(w, "list plan decisions", err)
		return
	}
	h, err := s.Queries.GetHousehold(r.Context(), identity.HouseholdID)
	if err != nil {
		s.internalError(w, "get plan review stamp", err)
		return
	}

	out := financialPlanResponse{
		Sections:  make([]planSectionResponse, 0, len(sectionRows)),
		Decisions: make([]planDecisionResponse, 0, len(decisionRows)),
	}
	for _, sec := range sectionRows {
		body, err := s.Cipher.OpenString(sec.Body)
		if err != nil {
			// Same posture as a transcript that will not open: returning half
			// a plan hides which parts are missing, so the whole read fails.
			s.internalError(w, "open plan section", err)
			return
		}
		resp := planSectionResponse{
			ID: sec.ID, Kind: sec.Kind, Body: body, PersonID: sec.PersonID,
			PersonName: sec.PersonName,
			UpdatedAt:  sec.UpdatedAt.UTC().Format(time.RFC3339),
		}
		out.Sections = append(out.Sections, resp)
	}
	for _, d := range decisionRows {
		body, err := s.Cipher.OpenString(d.Body)
		if err != nil {
			s.internalError(w, "open plan decision", err)
			return
		}
		out.Decisions = append(out.Decisions, planDecisionResponse{
			ID: d.ID, Topic: d.Topic, Body: body, Status: d.Status, Source: d.Source,
			DecidedAt:  d.DecidedAt.Format(time.DateOnly),
			Supersedes: d.Supersedes, Superseded: d.Superseded,
			CreatedAt: d.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	if h.PlanReviewedAt != nil {
		v := h.PlanReviewedAt.UTC().Format(time.RFC3339)
		out.ReviewedAt = &v
	}

	writeJSON(w, http.StatusOK, out)
}

// --------------------------------------------------------------------------
// PUT /api/plan/sections
// --------------------------------------------------------------------------

// validPlanSectionKinds is the fixed outline. A free-form page-tree is
// deliberately not offered: the advisor briefing digests these sections by
// kind, and a vocabulary the household invents at runtime is one nobody can
// digest.
var validPlanSectionKinds = map[string]bool{
	"strategy": true, "income": true, "estate": true, "person": true, "notes": true,
}

// maxPlanSectionBody bounds a section so the plan stays prose, not a pasted
// document archive — the vault exists for documents.
const maxPlanSectionBody = 50_000

type savePlanSectionRequest struct {
	Kind     string `json:"kind"`
	PersonID string `json:"person_id"`
	Body     string `json:"body"`
}

func (s *Server) handleSavePlanSection(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req savePlanSectionRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	kind := strings.TrimSpace(req.Kind)
	if !validPlanSectionKinds[kind] {
		writeError(w, http.StatusBadRequest,
			"kind must be strategy, income, estate, person, or notes")
		return
	}

	body := strings.TrimSpace(req.Body)
	if len(body) > maxPlanSectionBody {
		body = body[:maxPlanSectionBody]
	}

	// The person rule mirrors the table's CHECK as a readable 400 rather than
	// a 500 from the constraint: a person section needs a person, and no other
	// kind may be pinned to one.
	var personID *uuid.UUID
	if raw := strings.TrimSpace(req.PersonID); raw != "" {
		if kind != "person" {
			writeError(w, http.StatusBadRequest, "only the person kind takes a person_id")
			return
		}
		pid, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "person_id must be a UUID")
			return
		}
		// Household-scoped person resolution: a person id from another
		// household is not a person here, and 404 (not 403) for the same
		// reason every other scoped read gives it.
		if _, err := s.Queries.GetPerson(r.Context(), dbgen.GetPersonParams{
			ID: pid, HouseholdID: identity.HouseholdID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "person not found")
				return
			}
			s.internalError(w, "resolve person", err)
			return
		}
		personID = &pid
	} else if kind == "person" {
		writeError(w, http.StatusBadRequest, "the person kind requires a person_id")
		return
	}

	sealed, err := s.Cipher.SealString(body)
	if err != nil {
		s.internalError(w, "seal plan section", err)
		return
	}

	userID := identity.UserID
	sec, err := s.Queries.UpsertPlanSection(r.Context(), dbgen.UpsertPlanSectionParams{
		HouseholdID: identity.HouseholdID, Kind: kind, PersonID: personID,
		Body: sealed, UpdatedBy: &userID,
	})
	if err != nil {
		s.internalError(w, "save plan section", err)
		return
	}

	writeJSON(w, http.StatusOK, planSectionResponse{
		ID: sec.ID, Kind: sec.Kind, Body: body, PersonID: sec.PersonID,
		PersonName: nil, // filled by the list join; not needed on the save echo
		UpdatedAt:  sec.UpdatedAt.UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleDeletePlanSection(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	id, err := uuid.Parse(chi.URLParam(r, "sectionID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid section id")
		return
	}

	n, err := s.Queries.DeletePlanSection(r.Context(), dbgen.DeletePlanSectionParams{
		ID: id, HouseholdID: identity.HouseholdID,
	})
	if err != nil {
		s.internalError(w, "delete plan section", err)
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "section not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --------------------------------------------------------------------------
// POST /api/plan/decisions — create (confirmed, or a proposal)
// --------------------------------------------------------------------------

const (
	maxDecisionTopic = 200
	maxDecisionBody  = 20_000
)

var validDecisionStatuses = map[string]bool{"confirmed": true, "proposed": true}
var validDecisionSources = map[string]bool{"manual": true, "advisor": true}

type createPlanDecisionRequest struct {
	Topic     string `json:"topic"`
	Body      string `json:"body"`
	DecidedAt string `json:"decided_at"`
	// Status defaults to confirmed: a decision typed on the Plan page is made
	// the moment it is typed. 'proposed' is the suggestion tray — the advisor
	// chat drafts one and the household confirms it here.
	Status string `json:"status"`
	Source string `json:"source"`
	// Supersedes optionally names the decision this one replaces.
	Supersedes string `json:"supersedes"`
}

func (s *Server) handleCreatePlanDecision(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req createPlanDecisionRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	topic := strings.TrimSpace(req.Topic)
	if topic == "" {
		writeError(w, http.StatusBadRequest, "topic is required")
		return
	}
	if len(topic) > maxDecisionTopic {
		topic = topic[:maxDecisionTopic]
	}
	body := strings.TrimSpace(req.Body)
	if len(body) > maxDecisionBody {
		body = body[:maxDecisionBody]
	}

	// decided_at defaults to today: the common case is writing down a decision
	// as it is made. Backdating is legitimate (the plan is often written after
	// the fact) so no bound is placed on the past, and the future is refused
	// because a decision dated tomorrow is a plan for a decision, which is
	// what proposals are for.
	decided := time.Now().UTC()
	if raw := strings.TrimSpace(req.DecidedAt); raw != "" {
		parsed, err := time.Parse(time.DateOnly, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "decided_at must be YYYY-MM-DD")
			return
		}
		if parsed.After(decided.AddDate(0, 0, 1)) {
			writeError(w, http.StatusBadRequest, "decided_at cannot be in the future")
			return
		}
		decided = parsed
	}

	status := strings.TrimSpace(req.Status)
	if status == "" {
		status = "confirmed"
	}
	if !validDecisionStatuses[status] {
		writeError(w, http.StatusBadRequest, "status must be confirmed or proposed")
		return
	}
	source := strings.TrimSpace(req.Source)
	if source == "" {
		source = "manual"
	}
	if !validDecisionSources[source] {
		writeError(w, http.StatusBadRequest, "source must be manual or advisor")
		return
	}

	// The supersede path. Only a CONFIRMED decision may be superseded — a
	// proposal retiring a decision before anybody confirms the replacement is
	// the suggestion tray editing the log — and only a CONFIRMED decision may
	// supersede, for the mirror-image reason. Household scope on the target
	// comes from the scoped read, not from the insert.
	var supersedes *uuid.UUID
	if raw := strings.TrimSpace(req.Supersedes); raw != "" {
		sid, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "supersedes must be a decision id")
			return
		}
		target, err := s.Queries.GetPlanDecision(r.Context(), dbgen.GetPlanDecisionParams{
			ID: sid, HouseholdID: identity.HouseholdID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusBadRequest, "decision to supersede not found")
			return
		} else if err != nil {
			s.internalError(w, "resolve supersede target", err)
			return
		}
		if target.Status != "confirmed" {
			writeError(w, http.StatusBadRequest, "only a confirmed decision can be superseded")
			return
		}
		if status != "confirmed" {
			writeError(w, http.StatusBadRequest, "only a confirmed decision can supersede another")
			return
		}
		supersedes = &sid
	}

	sealed, err := s.Cipher.SealString(body)
	if err != nil {
		s.internalError(w, "seal plan decision", err)
		return
	}

	userID := identity.UserID
	d, err := s.Queries.InsertPlanDecision(r.Context(), dbgen.InsertPlanDecisionParams{
		HouseholdID: identity.HouseholdID, Topic: topic, Body: sealed,
		DecidedAt: decided, Status: status, Supersedes: supersedes,
		Source: source, CreatedBy: &userID,
	})
	if err != nil {
		s.internalError(w, "create plan decision", err)
		return
	}

	writeJSON(w, http.StatusCreated, planDecisionResponse{
		ID: d.ID, Topic: d.Topic, Body: body, Status: d.Status, Source: d.Source,
		DecidedAt: d.DecidedAt.Format(time.DateOnly), Supersedes: d.Supersedes,
		CreatedAt: d.CreatedAt.UTC().Format(time.RFC3339),
	})
}

// --------------------------------------------------------------------------
// PATCH/DELETE /api/plan/decisions/{id} — proposals only
// --------------------------------------------------------------------------

type updatePlanDecisionRequest struct {
	// Confirm promotes a proposal into the log. Mutually exclusive in spirit
	// with editing: confirm alone, or edit alone — the client sends one.
	Confirm bool `json:"confirm"`
	// The edit fields apply to a proposal only; a confirmed row ignores them
	// with a 400 rather than silently, because "edit a decision" must never
	// quietly succeed against the log.
	Topic     *string `json:"topic"`
	Body      *string `json:"body"`
	DecidedAt *string `json:"decided_at"`
}

func (s *Server) handleUpdatePlanDecision(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	id, err := uuid.Parse(chi.URLParam(r, "decisionID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid decision id")
		return
	}

	var req updatePlanDecisionRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	existing, err := s.Queries.GetPlanDecision(r.Context(), dbgen.GetPlanDecisionParams{
		ID: id, HouseholdID: identity.HouseholdID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "decision not found")
		return
	} else if err != nil {
		s.internalError(w, "get plan decision", err)
		return
	}

	// The append-only rule, stated at the boundary: confirmed rows leave here
	// with a 400 naming the alternative, not a silent no-op.
	if existing.Status != "proposed" {
		writeError(w, http.StatusBadRequest,
			"a confirmed decision cannot be edited — add a decision that supersedes it")
		return
	}

	if req.Confirm {
		d, err := s.Queries.ConfirmPlanDecision(r.Context(), dbgen.ConfirmPlanDecisionParams{
			ID: id, HouseholdID: identity.HouseholdID,
		})
		if err != nil {
			s.internalError(w, "confirm plan decision", err)
			return
		}
		body, err := s.Cipher.OpenString(d.Body)
		if err != nil {
			s.internalError(w, "open plan decision", err)
			return
		}
		writeJSON(w, http.StatusOK, planDecisionResponse{
			ID: d.ID, Topic: d.Topic, Body: body, Status: d.Status, Source: d.Source,
			DecidedAt: d.DecidedAt.Format(time.DateOnly), Supersedes: d.Supersedes,
			CreatedAt: d.CreatedAt.UTC().Format(time.RFC3339),
		})
		return
	}

	topic := existing.Topic
	if req.Topic != nil {
		topic = strings.TrimSpace(*req.Topic)
		if topic == "" {
			writeError(w, http.StatusBadRequest, "topic cannot be empty")
			return
		}
		if len(topic) > maxDecisionTopic {
			topic = topic[:maxDecisionTopic]
		}
	}

	// Re-seal rather than carrying bytes forward: the edit rewrites the whole
	// body, and the row's single sealed column stays single.
	bodyPlain := ""
	if req.Body != nil {
		bodyPlain = strings.TrimSpace(*req.Body)
		if len(bodyPlain) > maxDecisionBody {
			bodyPlain = bodyPlain[:maxDecisionBody]
		}
	}
	sealed, err := s.Cipher.SealString(bodyPlain)
	if err != nil {
		s.internalError(w, "seal plan decision", err)
		return
	}

	decided := existing.DecidedAt
	if req.DecidedAt != nil && strings.TrimSpace(*req.DecidedAt) != "" {
		parsed, err := time.Parse(time.DateOnly, strings.TrimSpace(*req.DecidedAt))
		if err != nil {
			writeError(w, http.StatusBadRequest, "decided_at must be YYYY-MM-DD")
			return
		}
		if parsed.After(time.Now().UTC().AddDate(0, 0, 1)) {
			writeError(w, http.StatusBadRequest, "decided_at cannot be in the future")
			return
		}
		decided = parsed
	}

	d, err := s.Queries.UpdateProposedPlanDecision(r.Context(), dbgen.UpdateProposedPlanDecisionParams{
		ID: id, HouseholdID: identity.HouseholdID,
		Topic: topic, Body: sealed, DecidedAt: decided,
	})
	if err != nil {
		s.internalError(w, "update proposed decision", err)
		return
	}
	writeJSON(w, http.StatusOK, planDecisionResponse{
		ID: d.ID, Topic: d.Topic, Body: bodyPlain, Status: d.Status, Source: d.Source,
		DecidedAt: d.DecidedAt.Format(time.DateOnly), Supersedes: d.Supersedes,
		CreatedAt: d.CreatedAt.UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleDeletePlanDecision(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	id, err := uuid.Parse(chi.URLParam(r, "decisionID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid decision id")
		return
	}

	// Scoped read first so the refusal can name the real reason: a confirmed
	// decision is not deletable, and a cross-household id is "not found"
	// rather than "not deletable".
	existing, err := s.Queries.GetPlanDecision(r.Context(), dbgen.GetPlanDecisionParams{
		ID: id, HouseholdID: identity.HouseholdID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "decision not found")
		return
	} else if err != nil {
		s.internalError(w, "get plan decision", err)
		return
	}
	if existing.Status != "proposed" {
		writeError(w, http.StatusBadRequest,
			"a confirmed decision cannot be deleted — supersede it with a new one")
		return
	}

	n, err := s.Queries.DeleteProposedPlanDecision(r.Context(), dbgen.DeleteProposedPlanDecisionParams{
		ID: id, HouseholdID: identity.HouseholdID,
	})
	if err != nil {
		s.internalError(w, "delete proposed decision", err)
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "decision not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --------------------------------------------------------------------------
// POST /api/plan/review — the stamp
// --------------------------------------------------------------------------

func (s *Server) handleReviewPlan(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	h, err := s.Queries.StampPlanReview(r.Context(), identity.HouseholdID)
	if err != nil {
		s.internalError(w, "stamp plan review", err)
		return
	}
	out := struct {
		ReviewedAt string `json:"reviewed_at"`
	}{ReviewedAt: h.PlanReviewedAt.UTC().Format(time.RFC3339)}
	writeJSON(w, http.StatusOK, out)
}

// --------------------------------------------------------------------------
// The briefing digest
// --------------------------------------------------------------------------

// OpenPlanDigest decrypts and bounds the plan digest BuildBriefing carried
// sealed. The advisor package has no cipher by design — transcripts and plan
// bodies stay opaque until this layer — so the bodies travel sealed inside the
// Briefing and are opened exactly here, for both surfaces that consume it (the
// briefing endpoint and the advisor_briefing chat tool). One opener, so the
// two can never excerpt the plan differently.
//
// Bounds are about the model's attention, not secrecy: a digest that quotes the
// whole plan verbatim is a transcript, not a briefing. Strategy is worth the
// most room because it is the sentence every advisor answer should respect.
const (
	planDigestStrategyChars = 2000
	planDigestSectionChars  = 800
	planDigestPersonChars   = 400
	planDigestDecisionChars = 400
)

type planDigestView struct {
	Strategy  string
	Sections  []planDigestSection
	Persons   []planDigestSection
	Decisions []planDigestDecision
}

type planDigestSection struct {
	Kind string
	Who  string
	Body string
}

type planDigestDecision struct {
	Topic     string
	DecidedOn string
	Body      string
}

// openPlanDigest returns nil when the household has no plan — the callers
// render that as an absent field, never as a zero-valued "empty plan".
func (s *Server) openPlanDigest(d advisor.PlanDigest) *planDigestView {
	if !d.Exists {
		return nil
	}
	open := func(sealed []byte, limit int) string {
		plain, err := s.Cipher.OpenString(sealed)
		if err != nil {
			// A body that will not open is a key mismatch. The digest is
			// best-effort decoration on a briefing whose figures are already
			// computed — a missing excerpt must not blank the advisor — so an
			// unreadable body contributes nothing rather than failing the run.
			return ""
		}
		if len(plain) > limit {
			return plain[:limit] + "…"
		}
		return plain
	}

	out := &planDigestView{}
	for _, sec := range d.Sections {
		switch sec.Kind {
		case "strategy":
			out.Strategy = open(sec.Body, planDigestStrategyChars)
		case "person":
			out.Persons = append(out.Persons, planDigestSection{
				Kind: sec.Kind, Who: sec.PersonName,
				Body: open(sec.Body, planDigestPersonChars),
			})
		default:
			out.Sections = append(out.Sections, planDigestSection{
				Kind: sec.Kind,
				Body: open(sec.Body, planDigestSectionChars),
			})
		}
	}
	for _, dec := range d.Decisions {
		out.Decisions = append(out.Decisions, planDigestDecision{
			Topic: dec.Topic, DecidedOn: dec.DecidedAt.Format(time.DateOnly),
			Body: open(dec.Body, planDigestDecisionChars),
		})
	}
	return out
}

// planDigestToolResult renders the opened view for the advisor_briefing chat
// tool. The basis line is the whole reason the digest rides in the briefing:
// a model that knows the household's stated plan stops lecturing it about the
// three-month emergency fund the plan deliberately chose.
func planDigestToolResult(v *planDigestView) map[string]any {
	if v == nil {
		return nil
	}
	out := map[string]any{
		"basis": "The household's own written plan, quoted from the Plan page. " +
			"When an answer would contradict it, say so explicitly and name the plan — " +
			"the household wrote it deliberately. Do not restate figures from the plan " +
			"as if they were computed here; quote the wording.",
	}
	if v.Strategy != "" {
		out["strategy"] = v.Strategy
	}
	if len(v.Sections) > 0 {
		sections := make([]map[string]any, 0, len(v.Sections))
		for _, sec := range v.Sections {
			if sec.Body == "" {
				continue
			}
			sections = append(sections, map[string]any{
				"kind": sec.Kind, "body": sec.Body,
			})
		}
		if len(sections) > 0 {
			out["sections"] = sections
		}
	}
	if len(v.Persons) > 0 {
		persons := make([]map[string]any, 0, len(v.Persons))
		for _, p := range v.Persons {
			if p.Body == "" {
				continue
			}
			persons = append(persons, map[string]any{
				"who": p.Who, "body": p.Body,
			})
		}
		if len(persons) > 0 {
			out["persons"] = persons
		}
	}
	if len(v.Decisions) > 0 {
		decisions := make([]map[string]any, 0, len(v.Decisions))
		for _, d := range v.Decisions {
			decisions = append(decisions, map[string]any{
				"topic": d.Topic, "decided_on": d.DecidedOn,
				"body": d.Body,
			})
		}
		out["active_decisions"] = decisions
	}
	return out
}
