package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// Anomaly suppression: the "this merchant is fine, stop telling me" surface for
// the two detectors in internal/insights/anomaly.go. Modelled throughout on the
// recurring suppression handlers in report_handlers.go, including the decision
// to carry the merchant key on the query string for DELETE — keys can be long
// and contain URL-unfriendly characters.
//
// These are mounted inside the existing /api/insights group, which already
// carries auth.RequireAdult on the group. Suppression changes what the whole
// household's feed says, so it is adult-only, but it is not an operator action
// and does not want RequireOwner.

// anomalyScopes are the suppression scopes the anomaly_overrides CHECK allows.
// "this merchant charges odd amounts" and "this merchant bills me twice" are
// different claims, and silencing one must not silence the other.
var anomalyScopes = map[string]bool{"all": true, "outlier": true, "duplicate": true}

// anomalyKindScope maps an insight kind to the scope suppressing it implies.
// It doubles as the allowlist for the "this is normal" action: a kind that is
// not in here is not an anomaly and cannot be suppressed this way.
var anomalyKindScope = map[string]string{
	"merchant_outlier": "outlier",
	"duplicate_charge": "duplicate",
}

type suppressAnomalyRequest struct {
	MerchantKey string `json:"merchant_key"`
	Merchant    string `json:"merchant"`
	// Scope defaults to "all" — the single "never flag this merchant" the UI
	// offers. A caller wanting one detector says so explicitly.
	Scope string `json:"scope"`
}

func (s *Server) handleSuppressAnomaly(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req suppressAnomalyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.MerchantKey == "" {
		writeError(w, http.StatusBadRequest, "merchant_key is required")
		return
	}
	if req.Scope == "" {
		req.Scope = "all"
	}
	if !anomalyScopes[req.Scope] {
		writeError(w, http.StatusBadRequest, "scope must be one of: all, outlier, duplicate")
		return
	}

	if err := s.Queries.SuppressAnomalyMerchant(r.Context(), dbgen.SuppressAnomalyMerchantParams{
		HouseholdID:   identity.HouseholdID,
		MerchantKey:   req.MerchantKey,
		MerchantLabel: req.Merchant,
		Scope:         req.Scope,
	}); err != nil {
		s.internalError(w, "suppress anomaly merchant", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUnsuppressAnomaly(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	// The key can be long and contain URL-unfriendly characters, so it comes on
	// the query string rather than in the path.
	key := r.URL.Query().Get("merchant_key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "merchant_key is required")
		return
	}
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "all"
	}
	if !anomalyScopes[scope] {
		writeError(w, http.StatusBadRequest, "scope must be one of: all, outlier, duplicate")
		return
	}

	if err := s.Queries.UnsuppressAnomalyMerchant(r.Context(), dbgen.UnsuppressAnomalyMerchantParams{
		HouseholdID: identity.HouseholdID,
		MerchantKey: key,
		Scope:       scope,
	}); err != nil {
		s.internalError(w, "restore anomaly merchant", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type suppressedAnomalyResponse struct {
	// MerchantKey is the key suppression is recorded under — what unsuppress
	// takes. MerchantKeyResolved is the same key canonicalised, which is what
	// addresses the merchant detail view: a suppression recorded against a raw
	// descriptor before that descriptor was merged would otherwise link nowhere.
	MerchantKey         string `json:"merchant_key"`
	MerchantKeyResolved string `json:"merchant_key_resolved"`
	Merchant            string `json:"merchant"`
	Scope               string `json:"scope"`
	SuppressedAt        string `json:"suppressed_at"`
}

func (s *Server) handleListSuppressedAnomalies(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListAnomalyOverrides(r.Context(), identity.HouseholdID)
	if err != nil {
		s.internalError(w, "list suppressed anomalies", err)
		return
	}

	out := make([]suppressedAnomalyResponse, 0, len(rows))
	for _, m := range rows {
		out = append(out, suppressedAnomalyResponse{
			MerchantKey:         m.MerchantKey,
			MerchantKeyResolved: m.ResolvedMerchantKey,
			Merchant:            m.MerchantLabel,
			Scope:               m.Scope,
			SuppressedAt:        m.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleMarkInsightNormal is the affordance behind the feed's "This is normal"
// button: suppress the merchant for the detector that raised this insight, and
// dismiss the insight, in one request.
//
// The merchant key is read back out of the STORED insight rather than taken
// from the client. The key is a detail of how the insight was raised, not
// something a browser should be able to assert — and reading it server-side
// means the button cannot be used to suppress an arbitrary merchant.
func (s *Server) handleMarkInsightNormal(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	id, err := uuid.Parse(chi.URLParam(r, "insightID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid insight id")
		return
	}

	insight, err := s.Queries.GetInsight(r.Context(), dbgen.GetInsightParams{
		ID: id, HouseholdID: identity.HouseholdID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "insight not found")
			return
		}
		s.internalError(w, "load insight", err)
		return
	}

	scope, ok := anomalyKindScope[insight.Kind]
	if !ok {
		writeError(w, http.StatusBadRequest,
			"only anomaly insights can be marked normal; dismiss this one instead")
		return
	}

	// The payload is written by the producer, so these keys are guaranteed
	// present for the two anomaly kinds — but a stored row could predate a
	// payload change, so a missing key is a 400 rather than a panic.
	var payload struct {
		MerchantKey string `json:"merchant_key"`
		Merchant    string `json:"merchant"`
	}
	if err := json.Unmarshal(insight.Data, &payload); err != nil || payload.MerchantKey == "" {
		writeError(w, http.StatusBadRequest, "this insight has no merchant to suppress")
		return
	}

	if err := s.Queries.SuppressAnomalyMerchant(r.Context(), dbgen.SuppressAnomalyMerchantParams{
		HouseholdID:   identity.HouseholdID,
		MerchantKey:   payload.MerchantKey,
		MerchantLabel: payload.Merchant,
		Scope:         scope,
	}); err != nil {
		s.internalError(w, "suppress anomaly merchant", err)
		return
	}
	if err := s.Queries.DismissInsight(r.Context(), dbgen.DismissInsightParams{
		ID: id, HouseholdID: identity.HouseholdID,
	}); err != nil {
		s.internalError(w, "dismiss insight", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
