package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/ai"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/obligations"
)

type netWorthResponse struct {
	AssetsTotal      decimal.Decimal    `json:"assets_total"`
	LiabilitiesTotal decimal.Decimal    `json:"liabilities_total"`
	NetWorth         decimal.Decimal    `json:"net_worth"`
	Breakdown        networth.Breakdown `json:"breakdown"`
	AsOf             string             `json:"as_of"`
}

func (s *Server) handleNetWorth(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	result, err := networth.Compute(r.Context(), s.Queries, identity.HouseholdID)
	if err != nil {
		s.internalError(w, "compute net worth", err)
		return
	}

	writeJSON(w, http.StatusOK, netWorthResponse{
		AssetsTotal:      result.AssetsTotal,
		LiabilitiesTotal: result.LiabilitiesTotal,
		NetWorth:         result.NetWorth,
		Breakdown:        result.Breakdown,
		AsOf:             time.Now().Format(time.DateOnly),
	})
}

type snapshotResponse struct {
	AsOf             string          `json:"as_of"`
	AssetsTotal      decimal.Decimal `json:"assets_total"`
	LiabilitiesTotal decimal.Decimal `json:"liabilities_total"`
	NetWorth         decimal.Decimal `json:"net_worth"`
}

// handleNetWorthHistory returns the recorded trend, defaulting to two years.
func (s *Server) handleNetWorthHistory(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	q := r.URL.Query()
	to := parseDate(q.Get("to"), time.Now())
	from := parseDate(q.Get("from"), to.AddDate(-2, 0, 0))

	rows, err := s.Queries.ListNetWorthSnapshots(r.Context(), dbgen.ListNetWorthSnapshotsParams{
		HouseholdID: identity.HouseholdID,
		AsOf:        from,
		AsOf_2:      to,
	})
	if err != nil {
		s.internalError(w, "net worth history", err)
		return
	}

	out := make([]snapshotResponse, 0, len(rows))
	for _, s := range rows {
		out = append(out, snapshotResponse{
			AsOf:             s.AsOf.Format(time.DateOnly),
			AssetsTotal:      s.AssetsTotal,
			LiabilitiesTotal: s.LiabilitiesTotal,
			NetWorth:         s.NetWorth,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSnapshotNow records today's figure on demand, so a user does not have
// to wait for the scheduled job to see a new point appear.
func (s *Server) handleSnapshotNow(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	result, err := networth.Snapshot(r.Context(), s.Queries, identity.HouseholdID, nil)
	if err != nil {
		s.internalError(w, "snapshot net worth", err)
		return
	}

	writeJSON(w, http.StatusOK, netWorthResponse{
		AssetsTotal:      result.AssetsTotal,
		LiabilitiesTotal: result.LiabilitiesTotal,
		NetWorth:         result.NetWorth,
		Breakdown:        result.Breakdown,
		AsOf:             time.Now().Format(time.DateOnly),
	})
}

type holdingResponse struct {
	ID               uuid.UUID        `json:"id"`
	SecurityName     *string          `json:"security_name"`
	Ticker           *string          `json:"ticker"`
	SecurityType     *string          `json:"security_type"`
	Quantity         decimal.Decimal  `json:"quantity"`
	CostBasis        *decimal.Decimal `json:"cost_basis"`
	Value            *decimal.Decimal `json:"value"`
	Gain             *decimal.Decimal `json:"gain"`
	AccountName      string           `json:"account_name"`
	InstitutionName  *string          `json:"institution_name"`
	IsCashEquivalent bool             `json:"is_cash_equivalent"`
}

func (s *Server) handleListHoldings(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListVisibleHoldings(r.Context(), dbgen.ListVisibleHoldingsParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
	})
	if err != nil {
		s.internalError(w, "list holdings", err)
		return
	}

	out := make([]holdingResponse, 0, len(rows))
	for _, h := range rows {
		item := holdingResponse{
			ID:               h.ID,
			SecurityName:     h.SecurityName,
			Ticker:           h.Ticker,
			SecurityType:     h.SecurityType,
			Quantity:         h.Quantity,
			AccountName:      h.AccountName,
			InstitutionName:  h.InstitutionName,
			IsCashEquivalent: h.IsCashEquivalent,
			CostBasis:        nullDecimal(h.CostBasis),
			Value:            nullDecimal(h.InstitutionValue),
		}
		// Unrealised gain only means something when both sides are known.
		if h.InstitutionValue.Valid && h.CostBasis.Valid {
			gain := h.InstitutionValue.Decimal.Sub(h.CostBasis.Decimal)
			item.Gain = &gain
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, out)
}

type liabilityResponse struct {
	// ID is the ACCOUNT id. It used to be the liabilities row's own id, but most
	// debts have no liabilities row at all — Plaid serves that product for a
	// minority of institutions — and the account is what every consumer actually
	// keys on and what a payoff goal links to.
	ID uuid.UUID `json:"id"`
	// AccountID is what makes this list usable as a filter: a debt-payoff goal
	// links an account, and the picker needs to know which accounts are debts.
	AccountID       uuid.UUID `json:"account_id"`
	Kind            string    `json:"kind"`
	// AccountType is the raw 'credit' or 'loan', alongside Kind's display label.
	// A revolving debt and an amortizing one are not described the same way — on
	// a card the rate IS the APR, on a loan the APR is a disclosure figure and
	// the note rate is what accrues — so the UI has to be able to tell them
	// apart. Kind cannot answer that: it is Plaid's free-form subtype, and
	// enumerating which of its values revolve would be a second, guessier copy
	// of the rule isLiability() and ComputeNetWorth already share.
	AccountType     string    `json:"account_type"`
	AccountName     string    `json:"account_name"`
	Mask            *string   `json:"mask"`
	InstitutionName *string   `json:"institution_name"`
	APR             *decimal.Decimal `json:"apr"`
	// Balance is what is owed NOW — the account's current balance, not the last
	// statement balance liabilities.balance carries. This is the figure the
	// Accounts page shows and the one the Liabilities tile sums, so the debt
	// table and the total above it finally agree.
	Balance        *decimal.Decimal `json:"balance"`
	MinimumPayment *decimal.Decimal `json:"minimum_payment"`
	// Which source each figure came from: "manual", "plaid", or "" when nobody
	// knows it. The UI uses these to offer the entry form rather than rendering
	// an absent rate as a confident zero.
	APRSource string `json:"apr_source"`
	PaymentSource string `json:"payment_source"`
	// NextPaymentDueDate is the household's own scheduled bill where one exists,
	// falling back to whatever the institution reported. Derived from the
	// recurrence on every read rather than stored, so there is no next-due cache
	// to go stale.
	NextPaymentDueDate *string `json:"next_payment_due_date"`
	// The recurrence itself, so the form can round-trip what was set rather than
	// making the user re-choose it. Null when no bill is scheduled.
	Schedule  *paymentScheduleResponse `json:"schedule"`
	IsOverdue *bool                    `json:"is_overdue"`
}

// paymentScheduleResponse is the recurrence behind a debt's payment, mirroring
// recurring_obligations' (anchor, count, unit) model exactly — there is one
// cadence vocabulary in this app and this is not a second one.
type paymentScheduleResponse struct {
	ObligationID  uuid.UUID `json:"obligation_id"`
	AnchorDate    string    `json:"anchor_date"`
	IntervalCount int32     `json:"interval_count"`
	IntervalUnit  string    `json:"interval_unit"`
	// Label renders the cadence the way the rest of the UI says it ("monthly",
	// "every 2 weeks"), decided once in the obligations package.
	Label string `json:"label"`
}

// debtRow is one debt account as both liabilities queries return it.
//
// sqlc generates a distinct struct per query and the two projections are
// identical, so this is the one shape the renderer works against — a struct
// rather than a dozen positional arguments, because six consecutive
// decimal.NullDecimal parameters is an argument order waiting to be got wrong.
type debtRow struct {
	AccountID       uuid.UUID
	AccountName     string
	Mask            *string
	InstitutionName *string
	Subtype         *string
	AccountType     string
	CurrentBalance  decimal.NullDecimal

	ManualAPR              decimal.NullDecimal
	PlaidAPR               decimal.NullDecimal
	PlaidRate              decimal.NullDecimal
	ObligationAmount       decimal.NullDecimal
	ManualPayment          decimal.NullDecimal
	PlaidPayment           decimal.NullDecimal
	PlaidNextPaymentDueDate *time.Time
	IsOverdue              *bool
}

// liabilityFromRow renders one debt account with its merged terms.
//
// Shared by the list and the single-row read behind the terms endpoint, so a
// write's response can never describe a debt differently from the list it came
// from.
//
// scheduledDue and schedule come from the household's own linked bill and are
// nil when none is set. The scheduled date wins over the institution's for the
// same reason a typed rate wins over a reported one: it is the figure a person
// asserted, and a sync must not quietly talk over it.
func liabilityFromRow(
	row debtRow,
	scheduledDue *time.Time,
	schedule *paymentScheduleResponse,
) liabilityResponse {
	terms := mergeDebtTerms(
		row.ManualAPR, row.PlaidAPR, row.PlaidRate,
		row.ObligationAmount, row.ManualPayment, row.PlaidPayment,
	)

	item := liabilityResponse{
		ID:              row.AccountID,
		AccountID:       row.AccountID,
		Kind:            debtKindLabel(row.AccountType, row.Subtype),
		AccountType:     row.AccountType,
		AccountName:     row.AccountName,
		Mask:            row.Mask,
		InstitutionName: row.InstitutionName,
		Balance:         nullDecimal(row.CurrentBalance),
		APRSource:       terms.APRSource,
		PaymentSource:   terms.PaymentSource,
		Schedule:        schedule,
		IsOverdue:       row.IsOverdue,
	}
	// An unknown figure stays null rather than becoming zero — see the comment
	// on termSourceNone.
	if terms.APRSource != termSourceNone {
		apr := terms.APR
		item.APR = &apr
	}
	if terms.PaymentSource != termSourceNone {
		payment := terms.Payment
		item.MinimumPayment = &payment
	}
	switch {
	case scheduledDue != nil:
		due := scheduledDue.Format(time.DateOnly)
		item.NextPaymentDueDate = &due
	case row.PlaidNextPaymentDueDate != nil:
		due := row.PlaidNextPaymentDueDate.Format(time.DateOnly)
		item.NextPaymentDueDate = &due
	}
	return item
}

// scheduleFromObligation renders a linked bill's recurrence for the terms form
// to round-trip.
func scheduleFromObligation(o dbgen.RecurringObligation) *paymentScheduleResponse {
	return &paymentScheduleResponse{
		ObligationID:  o.ID,
		AnchorDate:    o.AnchorDate.Format(time.DateOnly),
		IntervalCount: o.IntervalCount,
		IntervalUnit:  o.IntervalUnit,
		Label:         obligations.Cadence{Count: o.IntervalCount, Unit: o.IntervalUnit}.Label(),
	}
}

func (s *Server) handleListLiabilities(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListVisibleLiabilities(r.Context(), dbgen.ListVisibleLiabilitiesParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
	})
	if err != nil {
		s.internalError(w, "list liabilities", err)
		return
	}

	// One extra round trip for every scheduled due date rather than one per
	// account: this list is short, but an N+1 over debts is still N+1.
	due, err := s.Queries.ListNextPaymentDueDates(r.Context(), dbgen.ListNextPaymentDueDatesParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Column3:     time.Now(),
	})
	if err != nil {
		s.internalError(w, "list payment due dates", err)
		return
	}
	dueByAccount := make(map[uuid.UUID]time.Time, len(due))
	for _, d := range due {
		dueByAccount[d.AccountID] = d.DueDate
	}

	out := make([]liabilityResponse, 0, len(rows))
	for _, l := range rows {
		var scheduled *time.Time
		if d, ok := dueByAccount[l.AccountID]; ok {
			scheduled = &d
		}
		out = append(out, liabilityFromRow(debtRowFromList(l), scheduled, nil))
	}
	writeJSON(w, http.StatusOK, out)
}

// The two projections are identical by construction; these adapters exist only
// because sqlc emits a separate struct per query.
func debtRowFromList(l dbgen.ListVisibleLiabilitiesRow) debtRow {
	return debtRow{
		AccountID: l.AccountID, AccountName: l.AccountName, Mask: l.Mask,
		InstitutionName: l.InstitutionName, Subtype: l.Subtype, AccountType: l.Type,
		CurrentBalance: l.CurrentBalance,
		ManualAPR:      l.ManualApr, PlaidAPR: l.Apr, PlaidRate: l.InterestRatePercentage,
		ObligationAmount: l.ObligationAmount,
		ManualPayment:    l.ManualMinimumPayment, PlaidPayment: l.MinimumPayment,
		PlaidNextPaymentDueDate: l.NextPaymentDueDate, IsOverdue: l.IsOverdue,
	}
}

func debtRowFromGet(l dbgen.GetVisibleLiabilityRow) debtRow {
	return debtRow{
		AccountID: l.AccountID, AccountName: l.AccountName, Mask: l.Mask,
		InstitutionName: l.InstitutionName, Subtype: l.Subtype, AccountType: l.Type,
		CurrentBalance: l.CurrentBalance,
		ManualAPR:      l.ManualApr, PlaidAPR: l.Apr, PlaidRate: l.InterestRatePercentage,
		ObligationAmount: l.ObligationAmount,
		ManualPayment:    l.ManualMinimumPayment, PlaidPayment: l.MinimumPayment,
		PlaidNextPaymentDueDate: l.NextPaymentDueDate, IsOverdue: l.IsOverdue,
	}
}

// accountTermsRequest is the whole state of one debt's terms, sent on every save.
//
// Both fields are nullable and null is meaningful: it means "I don't know this",
// which must not be stored as zero (see termSourceNone), and on an account that
// already has manual terms it is the only way to hand the figure back to
// whatever the institution reports. A typed value always beats a synced one, so
// without an explicit clear there would be no way out.
type accountTermsRequest struct {
	APR            *string `json:"apr"`
	MinimumPayment *string `json:"minimum_payment"`
	// Schedule is the recurring bill that pays this debt. Null clears it, which
	// deactivates the obligation rather than deleting it: the bill may already
	// have history on the calendar, and a schedule someone turned off is not the
	// same thing as one that never existed.
	Schedule *paymentScheduleRequest `json:"schedule"`
}

// paymentScheduleRequest is a recurrence in the app's only cadence vocabulary:
// an anchor date plus "every N days/weeks/months/years".
//
// Everything asked for is expressible in it. Monthly on the 15th is an anchor on
// a 15th with (1, month); weekly on Tuesdays is an anchor on a Tuesday with
// (1, week); fortnightly is (2, week); quarterly is (3, month). END OF MONTH is
// an anchor on a 31st: Postgres clamps 01-31 + 1 month to 02-28, and because the
// expansion computes anchor + n periods rather than stepping from the previous
// occurrence, March returns to the 31st instead of drifting off it forever.
type paymentScheduleRequest struct {
	// AnchorDate is any date the payment falls on — normally the next one due.
	AnchorDate    string `json:"anchor_date"`
	IntervalCount int32  `json:"interval_count"`
	IntervalUnit  string `json:"interval_unit"`
}

// maxManualAPR bounds a typed rate. It catches a fat finger, not an expensive
// loan — payday credit really does exceed 100% APR, so 100 would reject
// legitimate input. Mirrors the CHECK on account_terms.
const maxManualAPR = 200

// handleSetAccountTerms records what the household says a debt costs.
//
// This exists because Plaid serves the Liabilities product for a minority of
// institutions, so for most households there is no other way an APR ever reaches
// the payoff engine. Values written here survive every subsequent sync.
func (s *Server) handleSetAccountTerms(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	accountID, err := uuid.Parse(chi.URLParam(r, "accountID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid account id")
		return
	}

	var req accountTermsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Validated here rather than left to the CHECK constraints, so the user gets
	// a sentence that names the unit instead of a Postgres violation. The unit
	// hint is load-bearing: entering 0.1899 for 18.99% is the mistake people
	// make, it satisfies every constraint, and it silently reports a credit card
	// as very nearly interest-free.
	apr, bad := parseOptionalDecimal(req.APR, 0, maxManualAPR,
		`apr must be a percentage between 0 and 200 — enter "18.99" for 18.99%, not "0.1899"`)
	if bad != "" {
		writeError(w, http.StatusBadRequest, bad)
		return
	}
	payment, bad := parseOptionalDecimal(req.MinimumPayment, 0, 0,
		"minimum payment must be a positive amount")
	if bad != "" {
		writeError(w, http.StatusBadRequest, bad)
		return
	}

	var anchor time.Time
	if req.Schedule != nil {
		if !obligations.IsValidUnit(req.Schedule.IntervalUnit) {
			writeError(w, http.StatusBadRequest,
				`interval_unit must be "day", "week", "month" or "year"`)
			return
		}
		if req.Schedule.IntervalCount < 1 {
			writeError(w, http.StatusBadRequest, "interval_count must be at least 1")
			return
		}
		anchor, err = time.Parse(time.DateOnly, req.Schedule.AnchorDate)
		if err != nil {
			writeError(w, http.StatusBadRequest, "anchor_date must be a date like 2026-08-01")
			return
		}
		// A bill is an amount on a date; recurring_obligations.amount is NOT NULL
		// and CHECKed positive, so a schedule cannot be recorded without one.
		// Requiring it here gives a sentence rather than a constraint violation.
		if !payment.Valid || !payment.Decimal.IsPositive() {
			writeError(w, http.StatusBadRequest,
				"a payment schedule needs a monthly payment amount as well as a date")
			return
		}
	}

	// Whatever bill is already linked, so a re-save edits it in place rather than
	// leaving a trail of dead obligations on the calendar.
	existing, err := s.Queries.GetAccountTerms(r.Context(), dbgen.GetAccountTermsParams{
		AccountID:   accountID,
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
	})
	var linkedObligation *uuid.UUID
	switch {
	case err == nil:
		linkedObligation = existing.PaymentObligationID
	case errors.Is(err, pgx.ErrNoRows):
		// No terms yet, so no bill to reuse.
	default:
		s.internalError(w, "read account terms", err)
		return
	}

	obligationID, bad, err := s.syncPaymentObligation(
		r.Context(), identity, accountID, linkedObligation, req.Schedule, anchor, payment)
	if err != nil {
		s.internalError(w, "sync payment obligation", err)
		return
	}
	if bad != "" {
		writeError(w, http.StatusNotFound, bad)
		return
	}

	// Nothing left to say about this debt: drop the row rather than store one
	// that says nothing.
	if !apr.Valid && !payment.Valid && obligationID == nil {
		if err := s.Queries.DeleteAccountTerms(r.Context(), dbgen.DeleteAccountTermsParams{
			AccountID:   accountID,
			HouseholdID: identity.HouseholdID,
			UserID:      identity.UserID,
		}); err != nil {
			s.internalError(w, "delete account terms", err)
			return
		}
	} else if _, err := s.Queries.UpsertAccountTerms(r.Context(), dbgen.UpsertAccountTermsParams{
		AccountID:           accountID,
		HouseholdID:         identity.HouseholdID,
		UpdatedBy:           &identity.UserID,
		Apr:                 apr,
		MinimumPayment:      payment,
		PaymentObligationID: obligationID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The guard lives inside the INSERT ... SELECT, so no visible debt
			// account matched. One answer for "doesn't exist", "not yours",
			// "kept private" and "not a debt": telling them apart would leak
			// whether an id is real.
			writeError(w, http.StatusNotFound, "account not found")
			return
		}
		s.internalError(w, "upsert account terms", err)
		return
	}

	s.writeLiability(r.Context(), w, identity, accountID)
}

// writeLiability answers with one debt exactly as the list would describe it.
func (s *Server) writeLiability(ctx context.Context, w http.ResponseWriter, identity auth.Identity, accountID uuid.UUID) {
	row, err := s.Queries.GetVisibleLiability(ctx, dbgen.GetVisibleLiabilityParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		ID:          accountID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "account not found")
			return
		}
		s.internalError(w, "read account terms", err)
		return
	}

	terms, err := s.Queries.GetAccountTerms(ctx, dbgen.GetAccountTermsParams{
		AccountID:   accountID,
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
	})
	var (
		schedule *paymentScheduleResponse
		due      *time.Time
	)
	if err == nil && terms.PaymentObligationID != nil {
		o, err := s.Queries.GetObligation(ctx, dbgen.GetObligationParams{
			ID: *terms.PaymentObligationID, HouseholdID: identity.HouseholdID, UserID: &identity.UserID,
		})
		if err == nil {
			schedule = scheduleFromObligation(o)
		}
		dates, err := s.Queries.ListNextPaymentDueDates(ctx, dbgen.ListNextPaymentDueDatesParams{
			HouseholdID: identity.HouseholdID, UserID: identity.UserID, Column3: time.Now(),
		})
		if err == nil {
			for _, d := range dates {
				if d.AccountID == accountID {
					next := d.DueDate
					due = &next
					break
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, liabilityFromRow(debtRowFromGet(row), due, schedule))
}

// syncPaymentObligation brings the debt's linked bill in line with the request:
// created when a schedule is set for the first time, edited in place when it
// changes, and deactivated when it is cleared.
//
// Deactivated rather than deleted: the bill may already be on the household's
// calendar and referenced by a projection someone has looked at, and a schedule
// switched off is not the same fact as one that never existed. Returns the id to
// link, nil when there is none, and a non-empty bad string when the account is
// not one the caller may write to.
func (s *Server) syncPaymentObligation(
	ctx context.Context,
	identity auth.Identity,
	accountID uuid.UUID,
	linked *uuid.UUID,
	req *paymentScheduleRequest,
	anchor time.Time,
	amount decimal.NullDecimal,
) (*uuid.UUID, string, error) {
	// Cleared: retire whatever was linked.
	if req == nil {
		if linked == nil {
			return nil, "", nil
		}
		o, err := s.Queries.GetObligation(ctx, dbgen.GetObligationParams{
			ID: *linked, HouseholdID: identity.HouseholdID, UserID: &identity.UserID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, "", nil // already gone
			}
			return nil, "", err
		}
		if _, err := s.Queries.UpdateObligation(ctx, dbgen.UpdateObligationParams{
			ID: o.ID, HouseholdID: identity.HouseholdID, UserID: &identity.UserID,
			Label: o.Label, Amount: o.Amount, CategoryID: o.CategoryID, AccountID: o.AccountID,
			IntervalCount: o.IntervalCount, IntervalUnit: o.IntervalUnit,
			AnchorDate: o.AnchorDate, EndDate: o.EndDate,
			IsShared: o.IsShared, IsActive: false,
		}); err != nil {
			return nil, "", err
		}
		return nil, "", nil
	}

	label, err := s.debtPaymentLabel(ctx, identity, accountID)
	if err != nil {
		return nil, "", err
	}
	if label == "" {
		return nil, "account not found", nil
	}

	if linked != nil {
		if _, err := s.Queries.UpdateObligation(ctx, dbgen.UpdateObligationParams{
			ID: *linked, HouseholdID: identity.HouseholdID, UserID: &identity.UserID,
			Label: label, Amount: amount.Decimal, AccountID: &accountID,
			IntervalCount: req.IntervalCount, IntervalUnit: req.IntervalUnit,
			AnchorDate: anchor, IsShared: true, IsActive: true,
		}); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return nil, "", err
			}
			// The linked row vanished under us; fall through and make a new one.
		} else {
			return linked, "", nil
		}
	}

	created, err := s.Queries.CreateObligation(ctx, dbgen.CreateObligationParams{
		HouseholdID:   identity.HouseholdID,
		UserID:        &identity.UserID,
		IsShared:      true,
		Label:         label,
		Amount:        amount.Decimal,
		AccountID:     &accountID,
		IntervalCount: req.IntervalCount,
		IntervalUnit:  req.IntervalUnit,
		AnchorDate:    anchor,
	})
	if err != nil {
		return nil, "", err
	}
	return &created.ID, "", nil
}

// debtPaymentLabel names the bill after the debt it pays, so the calendar reads
// "Mortgage payment" rather than an opaque row. Returns "" when the account is
// not a debt the caller can write to — the same single answer the rest of this
// endpoint gives, so nothing leaks about which ids are real.
func (s *Server) debtPaymentLabel(ctx context.Context, identity auth.Identity, accountID uuid.UUID) (string, error) {
	row, err := s.Queries.GetVisibleLiability(ctx, dbgen.GetVisibleLiabilityParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID, ID: accountID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return row.AccountName + " payment", nil
}

// parseOptionalDecimal reads a nullable money/rate field from a request.
//
// An absent or empty string is a deliberate clear, not an error. max of 0 means
// "no upper bound"; the lower bound always applies. Returns a non-empty bad
// string for anything the caller can fix.
func parseOptionalDecimal(raw *string, min, max float64, bad string) (decimal.NullDecimal, string) {
	if raw == nil || *raw == "" {
		return decimal.NullDecimal{}, ""
	}
	v, err := decimal.NewFromString(*raw)
	if err != nil {
		return decimal.NullDecimal{}, bad
	}
	if v.LessThan(decimal.NewFromFloat(min)) {
		return decimal.NullDecimal{}, bad
	}
	if max > 0 && v.GreaterThan(decimal.NewFromFloat(max)) {
		return decimal.NullDecimal{}, bad
	}
	return decimal.NullDecimal{Decimal: v, Valid: true}, ""
}

type manualAssetResponse struct {
	ID          uuid.UUID       `json:"id"`
	Name        string          `json:"name"`
	Kind        string          `json:"kind"`
	Value       decimal.Decimal `json:"value"`
	IsLiability bool            `json:"is_liability"`
	AsOf        string          `json:"as_of"`
	Notes       *string         `json:"notes"`
}

func (s *Server) handleListManualAssets(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListManualAssets(r.Context(), identity.HouseholdID)
	if err != nil {
		s.internalError(w, "list manual assets", err)
		return
	}

	out := make([]manualAssetResponse, 0, len(rows))
	for _, a := range rows {
		out = append(out, manualAssetResponse{
			ID: a.ID, Name: a.Name, Kind: a.Kind, Value: a.Value,
			IsLiability: a.IsLiability, AsOf: a.AsOf.Format(time.DateOnly), Notes: a.Notes,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type manualAssetRequest struct {
	Name        string  `json:"name"`
	Kind        string  `json:"kind"`
	Value       string  `json:"value"`
	IsLiability bool    `json:"is_liability"`
	Notes       *string `json:"notes"`
}

func (s *Server) handleCreateManualAsset(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req manualAssetRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	// Values arrive as strings so they never pass through a JSON float.
	value, err := decimal.NewFromString(req.Value)
	if err != nil {
		writeError(w, http.StatusBadRequest, "value must be a decimal number, e.g. \"425000.00\"")
		return
	}
	if req.Kind == "" {
		req.Kind = "other"
	}

	asset, err := s.Queries.CreateManualAsset(r.Context(), dbgen.CreateManualAssetParams{
		HouseholdID: identity.HouseholdID,
		CreatedBy:   &identity.UserID,
		Name:        req.Name,
		Kind:        req.Kind,
		Value:       value,
		IsLiability: req.IsLiability,
		Notes:       req.Notes,
	})
	if err != nil {
		s.internalError(w, "create manual asset", err)
		return
	}

	writeJSON(w, http.StatusCreated, manualAssetResponse{
		ID: asset.ID, Name: asset.Name, Kind: asset.Kind, Value: asset.Value,
		IsLiability: asset.IsLiability, AsOf: asset.AsOf.Format(time.DateOnly), Notes: asset.Notes,
	})
}

func (s *Server) handleDeleteManualAsset(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	assetID, err := uuid.Parse(chi.URLParam(r, "assetID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid asset id")
		return
	}

	if err := s.Queries.DeleteManualAsset(r.Context(), dbgen.DeleteManualAssetParams{
		ID: assetID, HouseholdID: identity.HouseholdID,
	}); err != nil {
		s.internalError(w, "delete manual asset", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type projectionResponse struct {
	Assumptions struct {
		MonthlySurplus    decimal.Decimal `json:"monthly_surplus"`
		AnnualReturnRate  decimal.Decimal `json:"annual_return_rate"`
		AnnualDebtPaydown decimal.Decimal `json:"annual_debt_paydown"`
		Months            int             `json:"months"`
	} `json:"assumptions"`
	Points []networth.ProjectionPoint `json:"points"`
	// Estimate is always true. It is in the payload so a client cannot render
	// these numbers without the caveat travelling with them.
	Estimate bool   `json:"estimate"`
	Basis    string `json:"basis"`
	// Narrative is an AI-written phrasing of the same milestones, present only
	// when AI is enabled and the call succeeded. Nil is normal — the numbers and
	// the caveat render without it.
	Narrative *string `json:"narrative,omitempty"`
}

// handleProjection rolls the current position forward.
//
// Defaults come from the household's own trailing-twelve-month behaviour, so
// the projection starts from what actually happened rather than an invented
// figure. Every assumption is overridable by query parameter and echoed back.
func (s *Server) handleProjection(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()

	current, err := networth.Compute(ctx, s.Queries, identity.HouseholdID)
	if err != nil {
		s.internalError(w, "compute net worth", err)
		return
	}

	// Default surplus: the trailing year's average monthly leftover. Shared with
	// the narrator so the two never drift.
	now := time.Now()
	defaultSurplus, err := networth.DefaultMonthlySurplus(ctx, s.Queries, identity.HouseholdID, identity.UserID, now)
	if err != nil {
		s.internalError(w, "spending summary", err)
		return
	}

	q := r.URL.Query()
	assumptions := networth.Assumptions{
		MonthlySurplus:    parseDecimal(q.Get("monthly_surplus"), defaultSurplus),
		AnnualReturnRate:  parseDecimal(q.Get("annual_return_rate"), decimal.RequireFromString("0.06")),
		AnnualDebtPaydown: parseDecimal(q.Get("annual_debt_paydown"), decimal.Zero),
		Months:            parseInt(q.Get("months"), 120, 1, 600),
	}

	resp := projectionResponse{
		Points:   networth.Project(current, assumptions),
		Estimate: true,
		Basis:    "Straight-line projection from your current balances and trailing-12-month averages. Not a market forecast.",
	}
	resp.Assumptions.MonthlySurplus = assumptions.MonthlySurplus
	resp.Assumptions.AnnualReturnRate = assumptions.AnnualReturnRate
	resp.Assumptions.AnnualDebtPaydown = assumptions.AnnualDebtPaydown
	resp.Assumptions.Months = assumptions.Months

	// Narration is a best-effort supplement: only attempted when AI is on, and a
	// slow or failed call leaves Narrative nil so the table still renders.
	if s.AI.Enabled() {
		in := s.buildForecastInput(ctx, identity, resp.Points, assumptions, resp.Basis, now)
		if text, err := s.AI.ForecastNarration(ctx, in); err != nil {
			slog.Warn("forecast narration failed", "household_id", identity.HouseholdID, "error", err)
		} else if text != "" {
			resp.Narrative = &text
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// buildForecastInput assembles the finished figures the narrator quotes: the
// milestone net-worth values (read straight from the projection, never
// recomputed) and the discretionary "lever" the household could trim, computed
// in decimal from the same reporting queries the summary uses.
func (s *Server) buildForecastInput(
	ctx context.Context,
	identity auth.Identity,
	points []networth.ProjectionPoint,
	assumptions networth.Assumptions,
	basis string,
	now time.Time,
) ai.ForecastInput {
	in := ai.ForecastInput{
		MonthlySurplus:   assumptions.MonthlySurplus.StringFixed(2),
		AnnualReturnRate: assumptions.AnnualReturnRate.String(),
		Basis:            basis,
	}
	for _, i := range networth.ForecastMilestones {
		if i < len(points) {
			in.Milestones = append(in.Milestones, ai.ForecastMilestone{
				Months:   i + 1,
				Month:    points[i].Month,
				NetWorth: points[i].NetWorth.StringFixed(2),
			})
		}
	}
	if lever, ok := s.topDiscretionaryLever(ctx, identity, now); ok {
		in.Levers = append(in.Levers, lever)
	}
	return in
}

// leverFloorDollars keeps a trivially-small overage from being narrated as a
// "lever" — noise below this is not worth suggesting the reader cut.
const leverFloorDollars = 25

// topDiscretionaryLever finds the discretionary category the household is
// spending the most above its own 6-month average this month, and reports that
// overage as a monthly saving. The subtraction is done here in decimal; the
// model only quotes the finished figure.
func (s *Server) topDiscretionaryLever(ctx context.Context, identity auth.Identity, now time.Time) (ai.ForecastLever, bool) {
	mStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	mEnd := mStart.AddDate(0, 1, -1)

	thisMonth, err := s.Queries.GetSpendingByCategory(ctx, dbgen.GetSpendingByCategoryParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID, Date: mStart, Date_2: mEnd,
	})
	if err != nil {
		return ai.ForecastLever{}, false
	}
	averages, err := s.Queries.GetCategoryAverages(ctx, dbgen.GetCategoryAveragesParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID,
		Date: mStart.AddDate(0, -6, 0), Date_2: mStart.AddDate(0, 0, -1),
	})
	if err != nil {
		return ai.ForecastLever{}, false
	}
	avgBySlug := make(map[string]decimal.Decimal, len(averages))
	for _, a := range averages {
		avgBySlug[a.CategorySlug] = a.MonthlyAverage
	}

	floor := decimal.NewFromInt(leverFloorDollars)
	var best ai.ForecastLever
	var bestDelta decimal.Decimal
	for _, c := range thisMonth {
		if c.IsFixed {
			continue // only discretionary categories are truly a lever
		}
		avg, ok := avgBySlug[c.CategorySlug]
		if !ok || !avg.IsPositive() {
			continue
		}
		delta := c.Total.Sub(avg)
		if delta.LessThan(floor) || !delta.GreaterThan(bestDelta) {
			continue
		}
		bestDelta = delta
		best = ai.ForecastLever{
			Label:          c.CategoryName,
			MonthlySavings: delta.Round(2).StringFixed(2),
			Basis:          "your 6-month average",
		}
	}
	return best, bestDelta.IsPositive()
}

func parseDecimal(raw string, fallback decimal.Decimal) decimal.Decimal {
	if raw == "" {
		return fallback
	}
	v, err := decimal.NewFromString(raw)
	if err != nil {
		return fallback
	}
	return v
}
