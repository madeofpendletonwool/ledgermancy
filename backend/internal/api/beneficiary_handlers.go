package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
)

// Whose money is it: tagging an account with the person it is held FOR, and the
// per-person net-worth lens that falls out of it.

type beneficiaryRequest struct {
	// Null clears the tag. "This is not held for anyone in particular" is the
	// normal state for most accounts and must be expressible.
	PersonID *uuid.UUID `json:"person_id"`
}

type beneficiaryResponse struct {
	ID                  uuid.UUID  `json:"id"`
	Name                string     `json:"name"`
	BeneficiaryPersonID *uuid.UUID `json:"beneficiary_person_id"`
}

// handleSetAccountBeneficiary records the person an account exists for: a 529's
// beneficiary, the minor on a UTMA, the child on a custodial Roth.
//
// This is the 529 sense of "beneficiary" — whose money this is — and NOT the
// payable-on-death sense. It is also not joint ownership: a joint checking
// account has two adult owners and this column cannot express that. Adult
// accounts stay shared through plaid_items.is_shared exactly as before.
func (s *Server) handleSetAccountBeneficiary(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	accountID, err := uuid.Parse(chi.URLParam(r, "accountID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid account id")
		return
	}

	var req beneficiaryRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	row, err := s.Queries.SetAccountBeneficiary(r.Context(), dbgen.SetAccountBeneficiaryParams{
		ID:          accountID,
		HouseholdID: identity.HouseholdID,
		PersonID:    req.PersonID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Either the account is not in this household or the person is not —
		// the query's guard covers both. One answer for both: distinguishing
		// them would leak whether an id exists elsewhere.
		writeError(w, http.StatusNotFound, "account or person not found")
		return
	}
	if err != nil {
		s.internalError(w, "set account beneficiary", err)
		return
	}

	writeJSON(w, http.StatusOK, beneficiaryResponse{
		ID: row.ID, Name: row.Name, BeneficiaryPersonID: row.BeneficiaryPersonID,
	})
}

type personAccountResponse struct {
	ID              uuid.UUID `json:"id"`
	Name            string    `json:"name"`
	InstitutionName *string   `json:"institution_name"`
	Type            string    `json:"type"`
	Subtype         *string   `json:"subtype"`
	TaxTreatment    *string   `json:"tax_treatment"`
	Balance         *string   `json:"balance"`
	IsCustodial     bool      `json:"is_custodial"`
}

// handleListMyAccounts serves the accounts held for the caller.
//
// This is the child's read-only window onto their own 529 or UTMA: balances and
// nothing else. It carries no transactions, no holdings detail, and no
// household figures.
func (s *Server) handleListMyAccounts(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	if identity.PersonID == nil {
		writeError(w, http.StatusNotFound, "no person record for this login")
		return
	}

	rows, err := s.Queries.ListAccountsForPerson(r.Context(), dbgen.ListAccountsForPersonParams{
		HouseholdID:         identity.HouseholdID,
		BeneficiaryPersonID: identity.PersonID,
	})
	if err != nil {
		s.internalError(w, "list accounts for person", err)
		return
	}

	out := make([]personAccountResponse, 0, len(rows))
	for _, a := range rows {
		resp := personAccountResponse{
			ID:              a.ID,
			Name:            a.Name,
			InstitutionName: a.InstitutionName,
			Type:            a.Type,
			Subtype:         a.Subtype,
			TaxTreatment:    a.TaxTreatment,
			IsCustodial:     a.TaxTreatment != nil && networth.IsCustodial(*a.TaxTreatment),
		}
		if a.CurrentBalance.Valid {
			v := a.CurrentBalance.Decimal.StringFixed(2)
			resp.Balance = &v
		}
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, out)
}

type personNetWorthResponse struct {
	PersonID    uuid.UUID `json:"person_id"`
	PersonName  string    `json:"person_name"`
	IsDependent bool      `json:"is_dependent"`
	Age         *int      `json:"age"`
	// AccountTotal is every account held for this person. CustodialTotal is the
	// subset that is legally theirs — 529, UTMA/UGMA, Coverdell, custodial Roth,
	// Trump — and is excluded from the household's retirement nest egg.
	AccountTotal   string `json:"account_total"`
	CustodialTotal string `json:"custodial_total"`
	ManualTotal    string `json:"manual_total"`
	Total          string `json:"total"`
}

type netWorthByPersonResponse struct {
	People []personNetWorthResponse `json:"people"`
	// Assigned is the sum of everything tagged to a person. It is a BREAKDOWN
	// of household assets, never a new total: an untagged account is still a
	// household asset and simply does not appear here. The client shows
	// Unassigned so the two obviously reconcile.
	Assigned   string `json:"assigned"`
	Unassigned string `json:"unassigned"`
	Total      string `json:"total"`
}

// handleNetWorthByPerson breaks household assets down by the person they are
// held for.
//
// THE INVARIANT: this changes no total. ComputeNetWorth is untouched, a child's
// 529 was already in household assets and stays there, and tagging an account
// with a beneficiary must not move the household's net worth by a cent. This
// endpoint only says which slice belongs to whom.
func (s *Server) handleNetWorthByPerson(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()
	now := time.Now()

	rows, err := s.Queries.ListPersonAssetTotals(ctx, identity.HouseholdID)
	if err != nil {
		s.internalError(w, "person asset totals", err)
		return
	}

	people, err := s.Queries.ListPeople(ctx, identity.HouseholdID)
	if err != nil {
		s.internalError(w, "list people", err)
		return
	}
	birthdates := map[uuid.UUID]*time.Time{}
	for _, p := range people {
		birthdates[p.ID] = p.Birthdate
	}

	manual, err := s.Queries.SumManualAssetsByPerson(ctx, identity.HouseholdID)
	if err != nil {
		s.internalError(w, "manual assets by person", err)
		return
	}
	manualByPerson := map[uuid.UUID]decimal.Decimal{}
	for _, m := range manual {
		if m.PersonID != nil {
			manualByPerson[*m.PersonID] = m.Total
		}
	}

	resp := netWorthByPersonResponse{People: []personNetWorthResponse{}}
	assigned := decimal.Zero

	for _, row := range rows {
		manualTotal := manualByPerson[row.PersonID]
		total := row.AccountTotal.Add(manualTotal)
		assigned = assigned.Add(total)

		resp.People = append(resp.People, personNetWorthResponse{
			PersonID:       row.PersonID,
			PersonName:     row.DisplayName,
			IsDependent:    row.IsDependent,
			Age:            AgeAt(birthdates[row.PersonID], now),
			AccountTotal:   row.AccountTotal.StringFixed(2),
			CustodialTotal: row.CustodialTotal.StringFixed(2),
			ManualTotal:    manualTotal.StringFixed(2),
			Total:          total.StringFixed(2),
		})
	}

	// The household total comes from the same engine the Net Worth page uses,
	// so the breakdown cannot disagree with the figure beside it.
	nw, err := networth.Compute(ctx, s.Queries, identity.HouseholdID)
	if err != nil {
		s.internalError(w, "compute net worth", err)
		return
	}

	resp.Assigned = assigned.StringFixed(2)
	resp.Total = nw.AssetsTotal.StringFixed(2)
	resp.Unassigned = nw.AssetsTotal.Sub(assigned).StringFixed(2)
	writeJSON(w, http.StatusOK, resp)
}
