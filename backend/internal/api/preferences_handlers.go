package api

import (
	"encoding/json"
	"net/http"

	"github.com/apex42group/ledgermancy/backend/internal/auth"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// Defaults for the reserved user-scoped keys, so the Settings UI always has
// something to render before a user has saved anything. The store itself is
// schemaless (any key/value); these are only the initial knobs 03/04/10 read.
// Values are raw JSON so they round-trip through the JSONB column unchanged.
var reservedUserPreferenceDefaults = map[string]json.RawMessage{
	"notify.channel":    json.RawMessage(`"none"`),
	"notify.ntfy_topic": json.RawMessage(`""`),
	"digest.enabled":    json.RawMessage(`false`),
	"digest.cadence":    json.RawMessage(`"weekly"`),
}

type preferencesResponse struct {
	User      map[string]json.RawMessage `json:"user"`
	Household map[string]json.RawMessage `json:"household"`
}

// handleGetPreferences returns the caller's resolved preferences: their
// user-scoped values (with reserved-key defaults filled in) plus their
// household-scoped values.
func (s *Server) handleGetPreferences(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	userRows, err := s.Queries.ListUserPreferences(r.Context(), &identity.UserID)
	if err != nil {
		s.internalError(w, "list user preferences", err)
		return
	}
	householdRows, err := s.Queries.ListHouseholdPreferences(r.Context(), &identity.HouseholdID)
	if err != nil {
		s.internalError(w, "list household preferences", err)
		return
	}

	// Start from the reserved defaults, then overlay stored values, so a key the
	// user never set still comes back with its default.
	user := make(map[string]json.RawMessage, len(reservedUserPreferenceDefaults)+len(userRows))
	for k, v := range reservedUserPreferenceDefaults {
		user[k] = v
	}
	for _, row := range userRows {
		user[row.Key] = json.RawMessage(row.Value)
	}

	household := make(map[string]json.RawMessage, len(householdRows))
	for _, row := range householdRows {
		household[row.Key] = json.RawMessage(row.Value)
	}

	writeJSON(w, http.StatusOK, preferencesResponse{User: user, Household: household})
}

type preferenceWrite struct {
	Scope string          `json:"scope"`
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

type upsertPreferencesRequest struct {
	Items []preferenceWrite `json:"items"`
}

// handleUpsertPreferences upserts one or many preferences for the caller. A
// user can only ever write their own user-scoped prefs or their own
// household's — the scope is honored but the owning ID always comes from the
// authenticated identity, never the request body, so cross-tenant writes are
// impossible by construction.
func (s *Server) handleUpsertPreferences(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req upsertPreferencesRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Items) == 0 {
		writeError(w, http.StatusBadRequest, "no preferences to write")
		return
	}

	for _, item := range req.Items {
		if item.Key == "" {
			writeError(w, http.StatusBadRequest, "preference key must not be empty")
			return
		}
		// The column is JSONB; reject anything that is not valid JSON before it
		// reaches the driver, so the error is a clean 400 rather than a 500.
		if len(item.Value) == 0 || !json.Valid(item.Value) {
			writeError(w, http.StatusBadRequest, "preference value must be valid JSON")
			return
		}

		switch item.Scope {
		case "user":
			_, err := s.Queries.UpsertUserPreference(r.Context(), dbgen.UpsertUserPreferenceParams{
				UserID: &identity.UserID,
				Key:    item.Key,
				Value:  []byte(item.Value),
			})
			if err != nil {
				s.internalError(w, "upsert user preference", err)
				return
			}
		case "household":
			_, err := s.Queries.UpsertHouseholdPreference(r.Context(), dbgen.UpsertHouseholdPreferenceParams{
				HouseholdID: &identity.HouseholdID,
				Key:         item.Key,
				Value:       []byte(item.Value),
			})
			if err != nil {
				s.internalError(w, "upsert household preference", err)
				return
			}
		default:
			writeError(w, http.StatusBadRequest, "scope must be \"user\" or \"household\"")
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}
