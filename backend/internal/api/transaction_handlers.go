package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// transactionFlagsRequest sets how a transaction is COUNTED, as distinct from
// what it says. Both fields are pointers so absent means "leave alone" and the
// caller can set either without a read-modify-write.
type transactionFlagsRequest struct {
	// ExcludedFromReports hides the row from every report. Use for a duplicate,
	// or a charge that is not really the household's money.
	ExcludedFromReports *bool `json:"excluded_from_reports"`
	// IsOneTime keeps the row in the month it fell but takes it out of the
	// trailing averages that predict future months — a loan payoff, a tax bill, a
	// car purchase. This is the flag that stops one unusual month from rewriting
	// a household's idea of its fixed costs for the next six.
	IsOneTime *bool `json:"is_one_time"`
}

type transactionFlagsResponse struct {
	ID                  uuid.UUID `json:"id"`
	ExcludedFromReports bool      `json:"excluded_from_reports"`
	IsOneTime           bool      `json:"is_one_time"`
}

// handleSetTransactionFlags records a judgement about how a transaction should
// count. Unlike the manual-row editors it accepts Plaid-synced rows: a synced
// loan payoff is the case these flags exist for, and the sync upsert preserves
// both columns so the choice survives redelivery.
func (s *Server) handleSetTransactionFlags(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	transactionID, err := uuid.Parse(chi.URLParam(r, "transactionID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid transaction id")
		return
	}

	var req transactionFlagsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.ExcludedFromReports == nil && req.IsOneTime == nil {
		writeError(w, http.StatusBadRequest, "no flags given")
		return
	}

	// The UPDATE is scoped by household, so a caller cannot flag someone else's
	// transaction even with a valid id.
	updated, err := s.Queries.SetTransactionFlags(r.Context(), dbgen.SetTransactionFlagsParams{
		ID:                  transactionID,
		HouseholdID:         identity.HouseholdID,
		ExcludedFromReports: req.ExcludedFromReports,
		IsOneTime:           req.IsOneTime,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "transaction not found")
			return
		}
		s.internalError(w, "set transaction flags", err)
		return
	}

	writeJSON(w, http.StatusOK, transactionFlagsResponse{
		ID:                  updated.ID,
		ExcludedFromReports: updated.ExcludedFromReports,
		IsOneTime:           updated.IsOneTime,
	})
}
