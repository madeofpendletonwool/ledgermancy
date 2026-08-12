package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/goals"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
)

// The goal kinds. The first two are what 00012_goals.sql declared as a comment
// on a free TEXT column and goal_handlers.go enforced as a convention; the
// payoff arithmetic lives in goals.ComputePayoff.
//
// 'college' joins them in doc 32, and 00055_allocation_planner.sql turns the
// whole set into a database CHECK — so these three are now a schema invariant
// rather than a convention this file happens to keep.
const (
	goalKindSavings = "savings"
	goalKindPayoff  = "debt_payoff"
	goalKindCollege = "college"
)

// College goal defaults and bounds, matching the CHECK on goals.college_years.
// Four years is the convention, not the law: community-college transfers and
// five-year programmes exist, and the engine has no business assuming.
const (
	defaultCollegeYears = 4
	minCollegeYears     = 1
	maxCollegeYears     = 10
)

// goalResponse is one goal plus its DERIVED standing. current_amount and the
// feasibility figures are computed server-side (SQL + decimal), never stored, so
// they can't drift. Money is always fixed-2 strings.
//
// The top-level standing fields carry the same MEANING for both kinds, but not
// the same arithmetic: for a savings goal they come from goals.Compute
// (accumulation), for a payoff goal from goals.ComputePayoff (amortization).
// required_monthly on a payoff goal is therefore interest-aware and is larger
// than remaining/months — which is the entire point of the split.
type goalResponse struct {
	ID              uuid.UUID  `json:"id"`
	Scope           string     `json:"scope"`
	PersonID        *uuid.UUID `json:"person_id"`
	Kind            string     `json:"kind"`
	Name            string     `json:"name"`
	TargetAmount    string     `json:"target_amount"`
	TargetDate      *string    `json:"target_date"`
	AccountID       *uuid.UUID `json:"account_id"`
	CategoryID      *uuid.UUID `json:"category_id"`
	CurrentAmount   string     `json:"current_amount"`
	RequiredMonthly string     `json:"required_monthly"`
	Shortfall       string     `json:"shortfall"`
	MonthsLeft      int        `json:"months_left"`
	OnTrack         bool       `json:"on_track"`
	OpenEnded       bool       `json:"open_ended"`
	Achieved        bool       `json:"achieved"`
	CreatedAt       string     `json:"created_at"`
	// Payoff is the amortization detail behind a debt_payoff goal, and nil for a
	// savings goal.
	Payoff *payoffResponse `json:"payoff,omitempty"`

	// CollegeYears is how many years of study a college goal funds. Present on
	// every kind because the column is NOT NULL; only a college goal reads it.
	CollegeYears int `json:"college_years"`
	// CollegeBasis is the sentence that stops target_amount being misread. For a
	// college goal the target is ONE YEAR in today's dollars, so the standing
	// above is progress toward one year — the multi-year inflated projection,
	// with the per-year shortfall, lives on the Advisor's allocator. Empty for
	// every other kind.
	CollegeBasis string `json:"college_basis,omitempty"`
	// Remind is the per-item reminders opt-out (MAD-85). On by default; the
	// payoff_progress producer skips a goal that has it off.
	Remind bool `json:"remind"`
}

// collegeBasis is rendered verbatim beside a college goal's standing.
const collegeBasis = "For a college goal the target is ONE YEAR'S cost in today's dollars, so the progress " +
	"above is against one year. The full multi-year projection — each year inflated separately, drawn down, with " +
	"the first year the money runs short — is on the Advisor page's allocator."

// payoffResponse is the debt-payoff schedule: what the debt costs to carry and
// when it ends. Every figure is deterministic — the model never sees this
// before it is finished.
type payoffResponse struct {
	// Available is false when there is no schedule to show: the goal's account is
	// unlinked, or nobody — neither the bank nor the household — has supplied a
	// monthly payment. Reason says which, so the UI explains itself rather than
	// rendering a confident row of zeros. required_monthly and target_reachable
	// stay meaningful even then — "you'd need $X a month to clear this by then"
	// does not depend on knowing the current payment.
	Available bool   `json:"available"`
	Reason    string `json:"reason"`

	// Balance is what is owed now — the account's current balance, not the
	// liability's last statement balance.
	Balance string `json:"balance"`
	// APR is a percentage. Fixed to THREE decimals on an amortizing loan and two
	// on a card: note rates are quoted in eighths, so a 6.775% mortgage renders
	// as "6.78%" at two places — a figure that appears nowhere on the statement
	// the household read it off, and that they will reasonably take for a
	// transcription error of ours.
	APR string `json:"apr"`
	// AccountType is 'credit' or 'loan', so the UI can name the rate correctly:
	// on a card the rate IS the APR, on a loan the APR is a closing disclosure
	// and this figure is the note rate. See money.ts isAmortizingDebt().
	AccountType     string `json:"account_type"`
	MonthlyPayment  string `json:"monthly_payment"`
	MonthlyInterest string `json:"monthly_interest"`

	// Where the rate and the payment came from: "manual", "plaid", or "" when
	// nobody knows. The UI says which, so a schedule built on a rate the user
	// typed is never mistaken for one the bank confirmed — and so an absent rate
	// reads as "add one" rather than as 0.00%.
	APRSource     string `json:"apr_source"`
	PaymentSource string `json:"payment_source"`

	// NeverPaysOff is the case worth shouting about: the payment is at or below
	// the interest, so the balance never falls. Months, total_interest and
	// payoff_date are then zero/null — there is no schedule.
	NeverPaysOff  bool    `json:"never_pays_off"`
	Months        int     `json:"months"`
	TotalInterest string  `json:"total_interest"`
	PayoffDate    *string `json:"payoff_date"`
	// Schedule is the per-month balance + interest series behind the totals
	// above, present only when the debt amortizes (NeverPaysOff false and the
	// starting balance was positive). Drives the amortization curve (item #10,
	// MAD-34). Nil when there is no schedule — the chart renders its empty
	// state rather than a curve that contradicts "never".
	Schedule []payoffSchedulePoint `json:"schedule,omitempty"`

	// RequiredMonthly is the smallest payment that clears the balance by the
	// target date; zero when the goal is open-ended.
	RequiredMonthly string `json:"required_monthly"`
	TargetReachable bool   `json:"target_reachable"`
}

// payoffSchedulePoint is one month of a debt-payoff amortization schedule,
// shipped as decimal strings so the chart never touches a float. The balance
// line declines to zero; the interest shaded area sums to total_interest.
type payoffSchedulePoint struct {
	// Month is the 1-based payment number.
	Month int `json:"month"`
	// Interest is the interest charged this month (decimal string).
	Interest string `json:"interest"`
	// Balance is what is still owed after this month's payment (decimal string,
	// never negative — the final point is "0.00").
	Balance string `json:"balance"`
}

// goalVisibility resolves the caller's identity into the three parameters every
// goal query takes. Kept in one place so no call site can forget one: omitting
// allPersonGoals would silently hide a child's goals from their parent, and
// omitting personID would hide a child's goals from the child.
//
// A child sees person-scoped goals only where the person is themselves. An
// adult sees all of them — a parent who cannot see their child's savings goal
// cannot help with it.
func goalVisibility(identity auth.Identity) (userID *uuid.UUID, personID *uuid.UUID, all bool) {
	return &identity.UserID, identity.PersonID, identity.IsAdult()
}

func (s *Server) handleListGoals(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()
	now := time.Now()

	userID, personID, all := goalVisibility(identity)
	rows, err := s.Queries.ListGoals(ctx, dbgen.ListGoalsParams{
		HouseholdID:    identity.HouseholdID,
		UserID:         userID,
		PersonID:       personID,
		AllPersonGoals: all,
	})
	if err != nil {
		s.internalError(w, "list goals", err)
		return
	}

	out := make([]goalResponse, 0, len(rows))
	for _, g := range rows {
		resp, err := s.buildGoalResponse(ctx, g, now)
		if err != nil {
			s.internalError(w, "derive goal standing", err)
			return
		}
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, out)
}

// buildGoalResponse derives progress and standing for one goal, dispatching on
// kind: savings goals accumulate toward a target, payoff goals amortize a debt
// down to nothing. The identifying fields are shared; the arithmetic is not.
func (s *Server) buildGoalResponse(ctx context.Context, g dbgen.Goal, now time.Time) (goalResponse, error) {
	resp := goalResponse{
		ID:           g.ID,
		Scope:        g.Scope,
		PersonID:     g.PersonID,
		Kind:         g.Kind,
		Name:         g.Name,
		TargetAmount: g.TargetAmount.StringFixed(2),
		AccountID:    g.AccountID,
		CategoryID:   g.CategoryID,
		CreatedAt:    g.CreatedAt.UTC().Format(time.RFC3339),
		Remind:       g.Remind,
	}
	if g.TargetDate != nil {
		d := g.TargetDate.Format(time.DateOnly)
		resp.TargetDate = &d
	}
	resp.CollegeYears = int(g.CollegeYears)
	if g.Kind == goalKindCollege {
		resp.CollegeBasis = collegeBasis
	}

	if g.Kind == goalKindPayoff {
		return s.fillPayoffStanding(ctx, g, resp, now)
	}
	return s.fillSavingsStanding(ctx, g, resp, now)
}

// fillSavingsStanding derives progress and feasibility for a savings goal.
// Progress is the linked account's balance, or accumulated surplus since
// created_at when unlinked. monthlySurplus is the same trailing-average the
// projection uses, so a goal's on-track decision agrees with the forecast the
// user already sees.
func (s *Server) fillSavingsStanding(ctx context.Context, g dbgen.Goal, resp goalResponse, now time.Time) (goalResponse, error) {
	current, err := s.goalProgress(ctx, g, now)
	if err != nil {
		return goalResponse{}, err
	}

	// Surplus scope matches the goal: a household goal reads shared cashflow; a
	// personal goal reads its owner's.
	//
	// A person-scoped goal reads household cashflow (uuid.Nil). A child has no
	// income or spending in the ledger — their money lives in the allowance
	// ledger, which is deliberately not joined to transactions — so scoping to
	// them would make every such goal read a surplus of zero and report
	// "never" as its completion date.
	surplusScope := uuid.Nil
	if g.Scope == "user" && g.UserID != nil {
		surplusScope = *g.UserID
	}
	surplus, err := networth.DefaultMonthlySurplus(ctx, s.Queries, g.HouseholdID, surplusScope, now)
	if err != nil {
		return goalResponse{}, err
	}

	f := goals.Compute(g.TargetAmount, current, surplus, g.TargetDate, now)

	resp.CurrentAmount = current.StringFixed(2)
	resp.RequiredMonthly = f.RequiredMonthly.StringFixed(2)
	resp.Shortfall = f.Shortfall.StringFixed(2)
	resp.MonthsLeft = f.MonthsLeft
	resp.OnTrack = f.OnTrack
	resp.OpenEnded = f.OpenEnded
	resp.Achieved = f.Achieved
	return resp, nil
}

// fillPayoffStanding derives the amortization standing for a debt-payoff goal.
//
// target_amount on a payoff goal is THE BALANCE TO ELIMINATE, captured from the
// account when the goal was created — not zero. So progress is
// `target_amount − what is still owed`, which climbs from 0 to the target as the
// debt is paid, and the shared progress bar reads correctly for both kinds. See
// the note on goals.ComputePayoff; a reader arriving from the savings path will
// otherwise assume "current" means money accumulated.
//
// The payment the schedule assumes is the liability's minimum payment. Nothing
// here invents one: when it is unknown the response says so and only the
// interest-aware required payment is reported.
func (s *Server) fillPayoffStanding(ctx context.Context, g dbgen.Goal, resp goalResponse, now time.Time) (goalResponse, error) {
	p := &payoffResponse{}
	resp.Payoff = p
	resp.OpenEnded = g.TargetDate == nil

	// Creation requires the link, but account_id is ON DELETE SET NULL: unlinking
	// the account later leaves a payoff goal with nothing to amortize.
	if g.AccountID == nil {
		p.Reason = "This payoff goal isn't linked to a debt account any more, so there's no schedule to compute."
		return resp, nil
	}

	// One read for the balance and both sets of terms. ErrNoRows here means the
	// account is not visible to this household at all — a hard error, not the
	// "no terms" case. That distinction used to be blurred: the old
	// GetGoalLiability returned ErrNoRows both for an invisible account and for
	// the ordinary situation of Plaid not reporting terms, and treating the
	// latter as an error is what kept every debt out of the payoff picker.
	row, err := s.Queries.GetGoalDebtTerms(ctx, dbgen.GetGoalDebtTermsParams{
		ID: *g.AccountID, HouseholdID: g.HouseholdID,
	})
	if err != nil {
		return goalResponse{}, err
	}
	balance := row.Balance

	// Debt retired so far. Floored at zero so a balance that has grown past the
	// captured original renders an empty bar rather than a negative one.
	progress := g.TargetAmount.Sub(balance)
	if progress.IsNegative() {
		progress = decimal.Zero
	}
	resp.CurrentAmount = progress.StringFixed(2)

	terms := mergeDebtTerms(
		row.ManualApr, row.Apr, row.InterestRatePercentage,
		row.ObligationAmount, row.ManualMinimumPayment, row.MinimumPayment,
	)
	apr, payment := terms.APR, terms.Payment

	f := goals.ComputePayoff(balance, apr, payment, g.TargetDate, now)

	resp.MonthsLeft = f.MonthsLeft
	resp.OnTrack = f.OnTrack
	resp.Achieved = f.Achieved
	resp.OpenEnded = f.OpenEnded
	resp.RequiredMonthly = f.RequiredMonthly.StringFixed(2)
	// How much more per month than is currently going in — the same "what would
	// fix this" number the savings shortfall carries.
	shortfall := f.RequiredMonthly.Sub(payment)
	if shortfall.IsNegative() || !f.TargetReachable {
		shortfall = decimal.Zero
	}
	resp.Shortfall = shortfall.StringFixed(2)

	p.Balance = balance.StringFixed(2)
	p.AccountType = row.Type
	p.APR = apr.StringFixed(aprDecimals(row.Type))
	p.MonthlyPayment = payment.StringFixed(2)
	p.MonthlyInterest = f.MonthlyInterest.StringFixed(2)
	p.NeverPaysOff = f.NeverPaysOff
	p.Months = f.Months
	p.TotalInterest = f.TotalInterest.StringFixed(2)
	p.RequiredMonthly = f.RequiredMonthly.StringFixed(2)
	p.TargetReachable = f.TargetReachable
	p.APRSource = terms.APRSource
	p.PaymentSource = terms.PaymentSource
	// Surface the per-month series only when the simulation produced one. A
	// never-paying or already-cleared debt carries no schedule, and the chart's
	// empty state is the honest rendering for those — the headline already says
	// "never" / "achieved", a curve would muddy both.
	if !f.NeverPaysOff && len(f.Schedule) > 0 {
		schedule := make([]payoffSchedulePoint, len(f.Schedule))
		for i, s := range f.Schedule {
			schedule[i] = payoffSchedulePoint{
				Month:    s.Month,
				Interest: s.Interest.StringFixed(2),
				Balance:  s.Balance.StringFixed(2),
			}
		}
		p.Schedule = schedule
	}
	if f.PayoffDate != nil {
		d := f.PayoffDate.Format(time.DateOnly)
		p.PayoffDate = &d
	}

	// What is missing decides the sentence, and both branches point at the fix.
	// Most institutions report no terms at all, so this is the ordinary state of
	// a new payoff goal rather than an error — the copy has to read as an
	// invitation, not a fault.
	switch {
	case terms.PaymentSource == termSourceNone && terms.APRSource == termSourceNone:
		p.Reason = "We don't have a rate or a monthly payment for this account — most banks don't report them. " +
			"Add them on the Accounts page and this becomes a full payoff schedule with a date and a total interest cost."
	case terms.PaymentSource == termSourceNone:
		p.Reason = "We don't have a monthly payment for this account. " +
			"Add what you pay each month on the Accounts page to see a payoff date."
	default:
		// A known payment with no rate is a real, complete answer, not a
		// degraded one: ComputePayoff treats a zero APR as interest-free, which
		// is correct arithmetic for an interest-free debt and a floor for any
		// other. The UI notes the caveat off apr_source. This branch existed
		// before but was effectively unreachable, because a liabilities row
		// without an APR is rare; it is now the common case for a household
		// that types a payment and leaves the rate blank.
		p.Available = true
	}
	return resp, nil
}

// goalProgress derives current progress toward a goal without storing it.
func (s *Server) goalProgress(ctx context.Context, g dbgen.Goal, now time.Time) (decimal.Decimal, error) {
	if g.AccountID != nil {
		return s.Queries.GetGoalAccountBalance(ctx, dbgen.GetGoalAccountBalanceParams{
			ID: *g.AccountID, HouseholdID: g.HouseholdID,
		})
	}

	// Unlinked: household surplus (income − spending) accumulated since the goal
	// was created, using the same definitions as the spending summary.
	scope := uuid.Nil
	if g.Scope == "user" && g.UserID != nil {
		scope = *g.UserID
	}
	sum, err := s.Queries.GetSpendingSummary(ctx, dbgen.GetSpendingSummaryParams{
		HouseholdID: g.HouseholdID, UserID: scope, Date: g.CreatedAt, Date_2: now,
	})
	if err != nil {
		return decimal.Zero, err
	}
	surplus := sum.Income.Sub(sum.Spending)
	if surplus.IsNegative() {
		surplus = decimal.Zero
	}
	return surplus, nil
}

type upsertGoalRequest struct {
	Scope    string     `json:"scope"`     // "household" (default) | "user" | "person"
	PersonID *uuid.UUID `json:"person_id"` // required iff scope == "person"
	// "savings" (default) | "debt_payoff" | "college". A payoff goal must link an
	// account that has a liabilities row; its target_amount may be omitted on
	// create and is then captured from that account's current balance.
	//
	// For a COLLEGE goal, target_amount is ONE YEAR'S cost in today's dollars,
	// not the four-year total. Storing the total instead would make the per-year
	// shortfall ("funded through sophomore year") impossible to compute.
	Kind         string     `json:"kind"`
	Name         string     `json:"name"`
	TargetAmount string     `json:"target_amount"`
	TargetDate   string     `json:"target_date"` // YYYY-MM-DD, optional
	AccountID    *uuid.UUID `json:"account_id"`
	CategoryID   *uuid.UUID `json:"category_id"`
	// CollegeYears defaults to 4 when absent. Ignored for other kinds.
	CollegeYears *int `json:"college_years"`
}

// validateCollegeYears bounds the study length, matching the column's CHECK so
// a bad value is a readable 400 rather than a 500 from a constraint violation.
func validateCollegeYears(raw *int) (int, error) {
	if raw == nil {
		return defaultCollegeYears, nil
	}
	if *raw < minCollegeYears || *raw > maxCollegeYears {
		return 0, errors.New("college_years must be between 1 and 10")
	}
	return *raw, nil
}

// validateGoalBody parses and checks a create/update body, returning the pieces
// the queries need. Amounts are decimal strings so they never pass through a
// float; the date, when present, must parse as a calendar date.
func validateGoalBody(req upsertGoalRequest) (amount decimal.Decimal, date *time.Time, scope, kind string, err error) {
	if len(req.Name) == 0 {
		return decimal.Zero, nil, "", "", errors.New("name is required")
	}
	amount, err = decimal.NewFromString(req.TargetAmount)
	if err != nil {
		return decimal.Zero, nil, "", "", errors.New("target_amount must be a decimal number, e.g. \"10000.00\"")
	}
	if !amount.IsPositive() {
		return decimal.Zero, nil, "", "", errors.New("target_amount must be greater than zero")
	}
	if req.TargetDate != "" {
		t, perr := time.Parse(time.DateOnly, req.TargetDate)
		if perr != nil {
			return decimal.Zero, nil, "", "", errors.New("target_date must be formatted YYYY-MM-DD")
		}
		date = &t
	}
	scope = req.Scope
	if scope == "" {
		scope = "household"
	}
	switch scope {
	case "household", "user":
		if req.PersonID != nil {
			return decimal.Zero, nil, "", "", errors.New("person_id is only valid with scope \"person\"")
		}
	case "person":
		// Mirrors the goals_scope_target constraint, so a missing person is a
		// readable 400 rather than a constraint violation.
		if req.PersonID == nil {
			return decimal.Zero, nil, "", "", errors.New("person_id is required for a person-scoped goal")
		}
	default:
		return decimal.Zero, nil, "", "", errors.New("scope must be \"household\", \"user\" or \"person\"")
	}
	kind = req.Kind
	if kind == "" {
		kind = goalKindSavings
	}
	if kind != goalKindSavings && kind != goalKindPayoff && kind != goalKindCollege {
		return decimal.Zero, nil, "", "", errors.New("kind must be \"savings\", \"debt_payoff\" or \"college\"")
	}
	return amount, date, scope, kind, nil
}

// resolvePayoffTarget prepares a debt-payoff goal for creation: it insists on a
// linked account that is actually a debt, and captures the balance to eliminate
// when the client didn't supply one.
//
// The gate is the account TYPE, not whether Plaid served loan terms for it. It
// used to be the latter, which is a different question with a different answer:
// Plaid serves the Liabilities product for a minority of institutions, so a
// household with a mortgage and two credit cards could not create a single
// payoff goal. An account with no reported rate is still a debt, and
// fillPayoffStanding degrades honestly rather than refusing.
//
// Capturing the balance here rather than in the browser is what keeps progress
// honest. The target is the *original* balance; if it were re-read on every
// request, progress would sit at zero forever no matter how much had been paid.
//
// Returns a non-empty bad string for anything the caller can fix, and err only
// for a genuine database failure — the two get different status codes.
func (s *Server) resolvePayoffTarget(ctx context.Context, req *upsertGoalRequest, householdID uuid.UUID) (bad string, err error) {
	if req.AccountID == nil {
		return "a debt payoff goal must link the account it's paying off", nil
	}

	row, err := s.Queries.GetGoalDebtTerms(ctx, dbgen.GetGoalDebtTermsParams{
		ID: *req.AccountID, HouseholdID: householdID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Does not exist, or belongs to another household. One answer for
			// both: telling them apart leaks whether an id is real.
			return "that account isn't a debt account — pick a credit card or loan", nil
		}
		return "", err
	}
	if !isDebtAccountType(row.Type) {
		return "that account isn't a debt account — pick a credit card or loan", nil
	}
	if req.TargetAmount != "" {
		return "", nil
	}
	if !row.Balance.IsPositive() {
		return "that account has nothing owed on it, so there's nothing to pay off", nil
	}
	req.TargetAmount = row.Balance.StringFixed(2)
	return "", nil
}

func (s *Server) handleCreateGoal(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req upsertGoalRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Kind == goalKindPayoff {
		bad, err := s.resolvePayoffTarget(r.Context(), &req, identity.HouseholdID)
		if err != nil {
			s.internalError(w, "resolve payoff target", err)
			return
		}
		if bad != "" {
			writeError(w, http.StatusBadRequest, bad)
			return
		}
	}
	amount, date, scope, kind, err := validateGoalBody(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	collegeYears, err := validateCollegeYears(req.CollegeYears)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	years := int16(collegeYears)

	// Exactly one of user_id / person_id is set, per scope, satisfying the
	// table's goals_scope_target CHECK.
	var userID, personID *uuid.UUID
	switch scope {
	case "user":
		userID = &identity.UserID
	case "person":
		// Resolve through the household guard: a valid person id from another
		// household must not become a goal here.
		if _, err := s.Queries.GetPerson(r.Context(), dbgen.GetPersonParams{
			ID: *req.PersonID, HouseholdID: identity.HouseholdID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "person not found")
				return
			}
			s.internalError(w, "get person for goal", err)
			return
		}
		personID = req.PersonID
	}

	g, err := s.Queries.CreateGoal(r.Context(), dbgen.CreateGoalParams{
		HouseholdID:  identity.HouseholdID,
		Scope:        scope,
		UserID:       userID,
		PersonID:     personID,
		Kind:         kind,
		Name:         req.Name,
		TargetAmount: amount,
		TargetDate:   date,
		AccountID:    req.AccountID,
		CategoryID:   req.CategoryID,
		CollegeYears: &years,
	})
	if err != nil {
		s.internalError(w, "create goal", err)
		return
	}

	resp, err := s.buildGoalResponse(r.Context(), g, time.Now())
	if err != nil {
		s.internalError(w, "derive goal standing", err)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleUpdateGoal(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	goalID, err := uuid.Parse(chi.URLParam(r, "goalID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid goal id")
		return
	}

	var req upsertGoalRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	amount, date, _, _, err := validateGoalBody(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Absent leaves the stored value alone (the query COALESCEs it), so a client
	// that does not know about college goals cannot reset one to four years by
	// renaming it.
	var collegeYears *int16
	if req.CollegeYears != nil {
		years, err := validateCollegeYears(req.CollegeYears)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		y := int16(years)
		collegeYears = &y
	}

	userID, personID, all := goalVisibility(identity)
	g, err := s.Queries.UpdateGoal(r.Context(), dbgen.UpdateGoalParams{
		ID:             goalID,
		HouseholdID:    identity.HouseholdID,
		UserID:         userID,
		PersonID:       personID,
		AllPersonGoals: all,
		Name:           req.Name,
		TargetAmount:   amount,
		TargetDate:     date,
		AccountID:      req.AccountID,
		CategoryID:     req.CategoryID,
		CollegeYears:   collegeYears,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "goal not found")
		return
	}
	if err != nil {
		s.internalError(w, "update goal", err)
		return
	}

	resp, err := s.buildGoalResponse(r.Context(), g, time.Now())
	if err != nil {
		s.internalError(w, "derive goal standing", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleArchiveGoal(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	goalID, err := uuid.Parse(chi.URLParam(r, "goalID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid goal id")
		return
	}

	userID, personID, all := goalVisibility(identity)
	if err := s.Queries.ArchiveGoal(r.Context(), dbgen.ArchiveGoalParams{
		ID:             goalID,
		HouseholdID:    identity.HouseholdID,
		UserID:         userID,
		PersonID:       personID,
		AllPersonGoals: all,
	}); err != nil {
		s.internalError(w, "archive goal", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// goalProposalResponse is the confirm-before-save proposal from POST
// /api/goals/parse. Nothing is written; Confirm calls the existing create path.
type goalProposalResponse struct {
	Name         string  `json:"name"`
	TargetAmount string  `json:"target_amount"`
	TargetDate   *string `json:"target_date"`
	Kind         string  `json:"kind"`
	// AccountID and AccountName are set for a debt_payoff proposal: the debt the
	// sentence named, resolved server-side to a real account the household has.
	// The model names a debt; it never chooses an id.
	AccountID   *uuid.UUID `json:"account_id"`
	AccountName string     `json:"account_name"`
}

// goalDebtLabel is the one label a household debt is known by on both sides of
// the parse: it goes into the prompt and comes back in ParsedGoal.Debt, so the
// two must be generated here and nowhere else.
func goalDebtLabel(l dbgen.ListVisibleLiabilitiesRow) string {
	label := l.AccountName
	if l.Mask != nil && *l.Mask != "" {
		label += " ••" + *l.Mask
	}
	if l.InstitutionName != nil && *l.InstitutionName != "" {
		label += " (" + *l.InstitutionName + ")"
	}
	return label
}

func (s *Server) handleParseGoal(w http.ResponseWriter, r *http.Request) {
	if !s.AI.Enabled() {
		writeError(w, http.StatusServiceUnavailable, "AI features are not configured")
		return
	}
	identity := auth.MustFromContext(r.Context())

	var req struct {
		Text string `json:"text"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Text == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}

	// The household's real debts go into the prompt so a payoff sentence can only
	// name one the household actually has.
	liabs, err := s.Queries.ListVisibleLiabilities(r.Context(), dbgen.ListVisibleLiabilitiesParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
	})
	if err != nil {
		s.internalError(w, "list liabilities for goal parse", err)
		return
	}
	debts := make([]string, 0, len(liabs))
	byLabel := make(map[string]uuid.UUID, len(liabs))
	for _, l := range liabs {
		label := goalDebtLabel(l)
		debts = append(debts, label)
		byLabel[label] = l.AccountID
	}

	today := time.Now().Format("Monday, 2 January 2006")
	parsed, err := s.AI.ParseGoal(r.Context(), req.Text, today, debts)
	if err != nil {
		s.internalError(w, "parse goal", err)
		return
	}

	// The model chooses between the two kinds the app implements; anything else
	// it invents falls back to savings rather than reaching the create path as an
	// unknown kind.
	kind := goalKindSavings
	switch parsed.Kind {
	case goalKindPayoff:
		kind = goalKindPayoff
	case "unsupported":
		reason := strings.TrimSpace(parsed.Reason)
		if reason == "" {
			reason = "That isn't a goal this app can set up yet."
		}
		writeError(w, http.StatusUnprocessableEntity, reason)
		return
	}

	// Re-validate the extracted figures in Go: a positive amount, and a date that
	// both parses and lies in the future. A failure is a clean 422 the UI shows
	// as "couldn't understand that", never a saved row.
	//
	// A payoff proposal is allowed to omit the amount: the balance to eliminate
	// comes from the account the user picks on confirm, not from the sentence.
	amount, err := decimal.NewFromString(parsed.TargetAmount)
	amountOK := err == nil && amount.IsPositive()
	if !amountOK && kind != goalKindPayoff {
		writeError(w, http.StatusUnprocessableEntity, "couldn't read a savings target from that")
		return
	}

	resp := goalProposalResponse{
		Name: parsed.Name,
		Kind: kind,
	}
	if amountOK {
		resp.TargetAmount = amount.StringFixed(2)
	}
	if kind == goalKindPayoff {
		// The label must round-trip exactly. A model that paraphrases the debt, or
		// names one the household doesn't have, gets the unsupported path — never a
		// goal quietly pointed at whichever debt happened to be first.
		accountID, ok := byLabel[strings.TrimSpace(parsed.Debt)]
		if !ok {
			writeError(w, http.StatusUnprocessableEntity,
				"couldn't match that to one of your debt accounts — pick it from the form instead")
			return
		}
		resp.AccountID = &accountID
		resp.AccountName = strings.TrimSpace(parsed.Debt)
	}
	if parsed.TargetDate != "" {
		d, derr := time.Parse(time.DateOnly, parsed.TargetDate)
		if derr != nil || !d.After(time.Now()) {
			writeError(w, http.StatusUnprocessableEntity, "the goal date must be a real date in the future")
			return
		}
		ds := d.Format(time.DateOnly)
		resp.TargetDate = &ds
	}
	writeJSON(w, http.StatusOK, resp)
}
