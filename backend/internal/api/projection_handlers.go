package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
)

// The Retirement surface: /api/projections.
//
// Two rules run through every handler here.
//
// First, the assumptions travel WITH the projection, always. A retirement curve
// rendered without the inputs that produced it is the exact thing doc 15 exists
// to prevent, and the only reliable way to guarantee the client has them is to
// make it impossible to fetch one without the other.
//
// Second, nothing on this surface is inferred. An untagged account is reported
// as excluded, an unconfigured tax year is reported as uncapped, an unreachable
// savings target is reported as unreachable. Every one of those could be papered
// over with a plausible default, and every one of those defaults would err in
// the flattering direction.

// --------------------------------------------------------------------------
// Assumptions
// --------------------------------------------------------------------------

type assumptionsResponse struct {
	RealReturnRate decimal.Decimal `json:"real_return_rate"`
	InflationRate  decimal.Decimal `json:"inflation_rate"`
	WithdrawalRate decimal.Decimal `json:"withdrawal_rate"`
	// Ages are null when not decided, which is a real answer and not a zero.
	TargetRetirementAge *int32 `json:"target_retirement_age"`
	CurrentAge          *int32 `json:"current_age"`

	AnnualSSIncome *decimal.Decimal `json:"annual_ss_income"`
	SSStartAge     *int32           `json:"ss_start_age"`

	// TargetAnnualSpending is what the user set. When null, the projection uses
	// DefaultedSpending — the household's own trailing spend — and says so.
	TargetAnnualSpending *decimal.Decimal `json:"target_annual_spending"`
	DefaultedSpending    decimal.Decimal  `json:"defaulted_spending"`
	SpendingIsDefaulted  bool             `json:"spending_is_defaulted"`

	// Basis is rendered verbatim beside the numbers.
	Basis string `json:"basis"`

	// MeasuredInflation is what CPI-U actually did over the trailing ten years,
	// annualised, beside the rate the household assumed (doc 27).
	//
	// It is SHOWN, never applied. Doc 27's rule is that no second inflation
	// input appears — InflationRate above stays the one the projection uses —
	// and this exists so choosing it is an informed act rather than accepting a
	// 3% default nobody has ever checked. Null when the series does not reach
	// back far enough.
	MeasuredInflation      *decimal.Decimal `json:"measured_inflation,omitempty"`
	MeasuredInflationYears int              `json:"measured_inflation_years,omitempty"`
	MeasuredInflationNote  string           `json:"measured_inflation_note,omitempty"`
}

// measuredInflationYears is the window the observed rate is taken over.
//
// Ten years, long enough to span a full cycle — the last decade contains both a
// near-zero-inflation stretch and the 2021-23 surge, and a shorter window would
// hand the user whichever of those it happened to land in as though it were the
// norm.
const measuredInflationYears = 10

const measuredInflationNote = "What CPI-U actually did over this window, annualised. Shown for comparison only — the projection uses the rate you set above, not this one."

// realBasis is the sentence that keeps the whole page honest. Everything is in
// today's dollars because every rate is real; a nominal return against today's
// spending is the classic way these projections flatter.
const realBasis = "Every figure is in today's dollars. The return rate is REAL — already net of inflation — so no number here needs discounting later."

// defaultAssumptions are used until the household saves its own.
//
// The figures come from networth.DefaultAssumptions rather than from literals
// here, because doc 32's allocator needs the same fallback for the same reason
// and two copies of "what we assume when nobody has said" is two pages quoting
// different projections from the same inputs.
func defaultAssumptions(householdID uuid.UUID) dbgen.ProjectionAssumption {
	d := networth.DefaultAssumptions()
	return dbgen.ProjectionAssumption{
		HouseholdID:          householdID,
		RealReturnRate:       d.RealReturnRate,
		InflationRate:        d.InflationRate,
		WithdrawalRate:       d.WithdrawalRate,
		CollegeInflationRate: d.CollegeInflationRate,
	}
}

// loadAssumptions reads the household's stored assumptions, falling back to the
// defaults when nothing has been saved. A household that has never opened the
// page still gets a working projection.
func (s *Server) loadAssumptions(ctx context.Context, householdID uuid.UUID) (dbgen.ProjectionAssumption, error) {
	row, err := s.Queries.GetProjectionAssumptions(ctx, householdID)
	if errors.Is(err, pgx.ErrNoRows) {
		return defaultAssumptions(householdID), nil
	}
	if err != nil {
		return dbgen.ProjectionAssumption{}, err
	}
	return row, nil
}

// defaultTargetSpending is the household's actual trailing-twelve-month spend.
//
// The app knows this figure, so it must not ask the user to guess it — the same
// principle DefaultMonthlySurplus already applies on the Net Worth page.
func (s *Server) defaultTargetSpending(
	ctx context.Context, identity auth.Identity, now time.Time,
) (decimal.Decimal, error) {
	summary, err := s.Queries.GetSpendingSummary(ctx, dbgen.GetSpendingSummaryParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        now.AddDate(-1, 0, 0),
		Date_2:      now,
	})
	if err != nil {
		return decimal.Zero, err
	}
	return summary.Spending.Round(2), nil
}

// measuredInflation is the trailing compound annual CPI-U change.
//
// Best-effort by design: it decorates the assumptions form and nothing depends
// on it, so a failed read or a series that does not reach back far enough
// returns nil and the page renders exactly as it did before. It must never turn
// a working projection into an error.
func (s *Server) measuredInflation(ctx context.Context, now time.Time) *decimal.Decimal {
	series, err := s.loadCPI(ctx)
	if err != nil || series.Empty() {
		return nil
	}
	base, ok := series.BasePeriod(now)
	if !ok {
		return nil
	}
	rate, ok := series.AnnualisedChange(base.AddDate(-measuredInflationYears, 0, 0), base)
	if !ok {
		return nil
	}
	rounded := rate.Round(4)
	return &rounded
}

func (s *Server) buildAssumptionsResponse(
	ctx context.Context, row dbgen.ProjectionAssumption, defaulted decimal.Decimal,
) assumptionsResponse {
	resp := assumptionsResponse{
		RealReturnRate:      row.RealReturnRate,
		InflationRate:       row.InflationRate,
		WithdrawalRate:      row.WithdrawalRate,
		TargetRetirementAge: row.TargetRetirementAge,
		CurrentAge:          row.CurrentAge,
		SSStartAge:          row.SsStartAge,
		AnnualSSIncome:      nullDecimal(row.AnnualSsIncome),
		DefaultedSpending:   defaulted,
		Basis:               realBasis,
	}
	if row.TargetAnnualSpending.Valid {
		v := row.TargetAnnualSpending.Decimal
		resp.TargetAnnualSpending = &v
	} else {
		resp.SpendingIsDefaulted = true
	}
	if measured := s.measuredInflation(ctx, time.Now()); measured != nil {
		resp.MeasuredInflation = measured
		resp.MeasuredInflationYears = measuredInflationYears
		resp.MeasuredInflationNote = measuredInflationNote
	}
	return resp
}

func (s *Server) handleGetAssumptions(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	row, err := s.loadAssumptions(r.Context(), identity.HouseholdID)
	if err != nil {
		s.internalError(w, "load projection assumptions", err)
		return
	}
	defaulted, err := s.defaultTargetSpending(r.Context(), identity, time.Now())
	if err != nil {
		s.internalError(w, "default target spending", err)
		return
	}

	writeJSON(w, http.StatusOK, s.buildAssumptionsResponse(r.Context(), row, defaulted))
}

// assumptionsRequest carries every field on every save, so the form is the whole
// state: clearing an age genuinely clears it rather than leaving a stale value
// behind a partial update.
//
// Rates and money arrive as strings so they never pass through a JSON float.
type assumptionsRequest struct {
	RealReturnRate       string  `json:"real_return_rate"`
	InflationRate        string  `json:"inflation_rate"`
	WithdrawalRate       string  `json:"withdrawal_rate"`
	TargetRetirementAge  *int    `json:"target_retirement_age"`
	CurrentAge           *int    `json:"current_age"`
	AnnualSSIncome       *string `json:"annual_ss_income"`
	SSStartAge           *int    `json:"ss_start_age"`
	TargetAnnualSpending *string `json:"target_annual_spending"`
}

// Bounds on the assumptions. These are not opinions about what a good rate is —
// they reject inputs that would make the engine produce nonsense (a negative
// withdrawal rate, an age of 400, a 900% return).
const (
	minAge         = 1
	maxAge         = 120
	maxRatePercent = 30 // 0.30
)

func (s *Server) handleSaveAssumptions(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req assumptionsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	realReturn, err := parseRate(req.RealReturnRate, "real_return_rate")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	inflation, err := parseRate(req.InflationRate, "inflation_rate")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	withdrawal, err := parseRate(req.WithdrawalRate, "withdrawal_rate")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !withdrawal.IsPositive() {
		writeError(w, http.StatusBadRequest, "withdrawal_rate must be greater than zero")
		return
	}

	params := dbgen.UpsertProjectionAssumptionsParams{
		HouseholdID:    identity.HouseholdID,
		RealReturnRate: realReturn,
		InflationRate:  inflation,
		WithdrawalRate: withdrawal,
	}

	for _, f := range []struct {
		name string
		src  *int
		dst  **int32
	}{
		{"target_retirement_age", req.TargetRetirementAge, &params.TargetRetirementAge},
		{"current_age", req.CurrentAge, &params.CurrentAge},
		{"ss_start_age", req.SSStartAge, &params.SsStartAge},
	} {
		v, err := optionalAge(f.src, f.name)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		*f.dst = v
	}

	if params.AnnualSsIncome, err = optionalMoney(req.AnnualSSIncome, "annual_ss_income"); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if params.TargetAnnualSpending, err = optionalMoney(req.TargetAnnualSpending, "target_annual_spending"); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	row, err := s.Queries.UpsertProjectionAssumptions(r.Context(), params)
	if err != nil {
		s.internalError(w, "save projection assumptions", err)
		return
	}
	defaulted, err := s.defaultTargetSpending(r.Context(), identity, time.Now())
	if err != nil {
		s.internalError(w, "default target spending", err)
		return
	}

	writeJSON(w, http.StatusOK, s.buildAssumptionsResponse(r.Context(), row, defaulted))
}

// --------------------------------------------------------------------------
// Contributions
// --------------------------------------------------------------------------

type contributionAccountResponse struct {
	ID              uuid.UUID        `json:"id"`
	Name            string           `json:"name"`
	Mask            *string          `json:"mask"`
	Subtype         *string          `json:"subtype"`
	InstitutionName *string          `json:"institution_name"`
	Balance         *decimal.Decimal `json:"balance"`
	TaxTreatment    *string          `json:"tax_treatment"`

	MonthlyContribution decimal.Decimal  `json:"monthly_contribution"`
	EmployerMatchPct    *decimal.Decimal `json:"employer_match_pct"`
	AnnualSalary        *decimal.Decimal `json:"annual_salary"`
	EmployerMatchLimit  *decimal.Decimal `json:"employer_match_limit"`

	BeneficiaryCurrentAge *int32 `json:"beneficiary_current_age"`
	BeneficiaryTargetAge  *int32 `json:"beneficiary_target_age"`

	// AssumedRealReturn is this account's own assumed real return (fraction),
	// null when unset — meaning the household rate is in effect.
	AssumedRealReturn *decimal.Decimal `json:"assumed_real_return"`

	// AnnualLimit is the IRS cap this account's kind is subject to, null when
	// none applies (taxable, 529, trust) or when the running year's limits are
	// not configured. LimitShared warns that the cap covers more than this one
	// account, so the headroom shown is the group's, not this account's alone.
	AnnualLimit   *decimal.Decimal `json:"annual_limit"`
	LimitShared   bool             `json:"limit_shared"`
	PlannedAnnual decimal.Decimal  `json:"planned_annual"`
}

type contributionsResponse struct {
	Accounts []contributionAccountResponse `json:"accounts"`
	// LimitsYear and LimitsConfigured let the UI say "2026 limits" — or say
	// plainly that it has none for this year rather than showing a stale cap.
	LimitsYear       int    `json:"limits_year"`
	LimitsConfigured bool   `json:"limits_configured"`
	LimitsNote       string `json:"limits_note"`
	UntaggedAccounts int    `json:"untagged_accounts"`
}

func (s *Server) handleListContributions(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()

	rows, err := s.Queries.ListProjectableAccounts(ctx, dbgen.ListProjectableAccountsParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID,
	})
	if err != nil {
		s.internalError(w, "list projectable accounts", err)
		return
	}
	assumptions, err := s.loadAssumptions(ctx, identity.HouseholdID)
	if err != nil {
		s.internalError(w, "load projection assumptions", err)
		return
	}

	year := time.Now().Year()
	limits, configured := networth.Limits(year)

	resp := contributionsResponse{
		Accounts:         make([]contributionAccountResponse, 0, len(rows)),
		LimitsYear:       year,
		LimitsConfigured: configured,
		LimitsNote:       limitsNote(year, configured),
	}

	age := 0
	if assumptions.CurrentAge != nil {
		age = int(*assumptions.CurrentAge)
	}

	for _, a := range rows {
		item := contributionAccountResponse{
			ID: a.ID, Name: a.Name, Mask: a.Mask, Subtype: a.Subtype,
			InstitutionName: a.InstitutionName, TaxTreatment: a.TaxTreatment,
			Balance:               nullDecimal(a.CurrentBalance),
			EmployerMatchPct:      nullDecimal(a.EmployerMatchPct),
			AnnualSalary:          nullDecimal(a.AnnualSalary),
			EmployerMatchLimit:    nullDecimal(a.EmployerMatchLimit),
			BeneficiaryCurrentAge: a.BeneficiaryCurrentAge,
			BeneficiaryTargetAge:  a.BeneficiaryTargetAge,
			AssumedRealReturn:     nullDecimal(a.AssumedRealReturn),
		}
		if a.MonthlyContribution.Valid {
			item.MonthlyContribution = a.MonthlyContribution.Decimal
		}
		item.PlannedAnnual = item.MonthlyContribution.Mul(decimal.NewFromInt(12)).Round(2)

		if a.TaxTreatment == nil {
			resp.UntaggedAccounts++
		} else if configured {
			if limit, capped := networth.AnnualLimitFor(*a.TaxTreatment, age, limits, false); capped {
				item.AnnualLimit = &limit
				item.LimitShared = true
			}
		}
		resp.Accounts = append(resp.Accounts, item)
	}

	writeJSON(w, http.StatusOK, resp)
}

func limitsNote(year int, configured bool) string {
	if configured {
		return "Headroom is against " + strconv.Itoa(year) + " IRS limits."
	}
	return "Limits for " + strconv.Itoa(year) + " aren't configured yet (we have them through " +
		strconv.Itoa(networth.LatestConfiguredYear()) +
		"), so contributions are projected uncapped. Treat the headroom figures as unavailable, not as unlimited."
}

type contributionRequest struct {
	MonthlyContribution   string  `json:"monthly_contribution"`
	EmployerMatchPct      *string `json:"employer_match_pct"`
	AnnualSalary          *string `json:"annual_salary"`
	EmployerMatchLimit    *string `json:"employer_match_limit"`
	BeneficiaryCurrentAge *int    `json:"beneficiary_current_age"`
	BeneficiaryTargetAge  *int    `json:"beneficiary_target_age"`
	// AssumedRealReturn is this account's OWN assumed real annual return, as a
	// fraction (0.06 = 6%). nil/empty means "use the household rate" — the
	// behaviour every account had before this field existed.
	AssumedRealReturn *string `json:"assumed_real_return"`
}

func (s *Server) handleSaveContribution(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	accountID, err := uuid.Parse(chi.URLParam(r, "accountID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid account id")
		return
	}

	var req contributionRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	monthly, err := decimal.NewFromString(req.MonthlyContribution)
	if err != nil {
		writeError(w, http.StatusBadRequest, "monthly_contribution must be a decimal number, e.g. \"500.00\"")
		return
	}
	if monthly.IsNegative() {
		writeError(w, http.StatusBadRequest, "monthly_contribution cannot be negative")
		return
	}

	params := dbgen.UpsertAccountContributionParams{
		AccountID:           accountID,
		HouseholdID:         identity.HouseholdID,
		UserID:              identity.UserID,
		MonthlyContribution: monthly,
	}

	if params.EmployerMatchPct, err = optionalRate(req.EmployerMatchPct, "employer_match_pct"); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if params.AnnualSalary, err = optionalMoney(req.AnnualSalary, "annual_salary"); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if params.EmployerMatchLimit, err = optionalMoney(req.EmployerMatchLimit, "employer_match_limit"); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if params.AssumedRealReturn, err = optionalRate(req.AssumedRealReturn, "assumed_real_return"); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	for _, f := range []struct {
		name string
		src  *int
		dst  **int32
	}{
		{"beneficiary_current_age", req.BeneficiaryCurrentAge, &params.BeneficiaryCurrentAge},
		{"beneficiary_target_age", req.BeneficiaryTargetAge, &params.BeneficiaryTargetAge},
	} {
		v, err := optionalAge(f.src, f.name)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		*f.dst = v
	}

	// An employer match percentage with no salary behind it is not an amount.
	// Rejecting it here is better than silently ignoring it in the engine and
	// leaving the user to wonder why their match never appears.
	if params.EmployerMatchPct.Valid && !params.AnnualSalary.Valid {
		writeError(w, http.StatusBadRequest,
			"an employer match percentage needs an annual salary to apply to — it is a percentage of salary, not of your contribution")
		return
	}

	// pgx.ErrNoRows here means the account is not visible to this caller: the
	// query's ownership join yielded nothing, so nothing was written.
	if _, err := s.Queries.UpsertAccountContribution(r.Context(), params); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "account not found")
		return
	} else if err != nil {
		s.internalError(w, "save account contribution", err)
		return
	}

	s.handleListContributions(w, r)
}

func (s *Server) handleDeleteContribution(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	accountID, err := uuid.Parse(chi.URLParam(r, "accountID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid account id")
		return
	}

	if err := s.Queries.DeleteAccountContribution(r.Context(), dbgen.DeleteAccountContributionParams{
		AccountID: accountID, HouseholdID: identity.HouseholdID, UserID: identity.UserID,
	}); err != nil {
		s.internalError(w, "delete account contribution", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --------------------------------------------------------------------------
// The projection itself
// --------------------------------------------------------------------------

type retirementResponse struct {
	// Assumptions travels with the curve, always. See the file comment.
	Assumptions       assumptionsResponse           `json:"assumptions"`
	Projection        networth.RetirementProjection `json:"projection"`
	Savings           *networth.SavingsRateSolve    `json:"required_savings,omitempty"`
	MonteCarlo        *networth.MonteCarloResult    `json:"monte_carlo,omitempty"`
	MonteCarloEnabled bool                          `json:"monte_carlo_enabled"`
	// Estimate is always true, in the payload so a client cannot render these
	// numbers without the caveat travelling with them — same contract as the
	// Net Worth projection.
	Estimate bool   `json:"estimate"`
	Basis    string `json:"basis"`
	// Omissions names what this model deliberately does not do, so the gaps are
	// the user's knowledge rather than their surprise.
	Omissions []string `json:"omissions"`
}

// retirementBasis and knownOmissions are rendered verbatim. The omissions list
// is doc 15's "out of scope" section turned into something the user actually
// sees: tax drag on withdrawals is real, large, and not modelled, and a user
// planning a traditional-heavy retirement needs to know that before they plan
// around this page.
const retirementBasis = "Each account is compounded separately at your real return rate, capped at its IRS contribution limit. Not a market forecast, and not advice."

var knownOmissions = []string{
	"Tax on withdrawals is not modelled. A traditional 401(k) dollar is worth less after tax than a Roth dollar, and this projection treats them alike.",
	"Required minimum distributions are not modelled.",
	"Returns are the flat real rate you set. Real markets do not deliver the same number every year, and the order they arrive in matters.",
}

func (s *Server) handleRetirementProjection(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()
	now := time.Now()

	stored, err := s.loadAssumptions(ctx, identity.HouseholdID)
	if err != nil {
		s.internalError(w, "load projection assumptions", err)
		return
	}
	defaulted, err := s.defaultTargetSpending(ctx, identity, now)
	if err != nil {
		s.internalError(w, "default target spending", err)
		return
	}
	rows, err := s.Queries.ListProjectableAccounts(ctx, dbgen.ListProjectableAccountsParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID,
	})
	if err != nil {
		s.internalError(w, "list projectable accounts", err)
		return
	}

	// The caller's own birthdate, when they have given one. A nil person or a
	// nil birthdate falls back to the stored current_age, so an instance that
	// has never set one projects exactly as it did before.
	var birthdate *time.Time
	if person, perr := s.Queries.GetPersonByUserID(ctx, &identity.UserID); perr == nil {
		birthdate = person.Birthdate
	} else if !errors.Is(perr, pgx.ErrNoRows) {
		s.internalError(w, "get person for projection", perr)
		return
	}

	assumptions := toRetirementAssumptions(stored, defaulted, birthdate, now)
	assumptions.Months = parseInt(r.URL.Query().Get("months"), defaultProjectionMonths(stored), 12, 720)
	assumptions.FamilyHSA = r.URL.Query().Get("family_hsa") == "true"

	plans := toAccountPlans(rows, now)

	resp := retirementResponse{
		Assumptions:       s.buildAssumptionsResponse(ctx, stored, defaulted),
		Projection:        networth.ProjectRetirement(plans, assumptions, now),
		MonteCarloEnabled: s.Config.Retirement.MonteCarloEnabled,
		Estimate:          true,
		Basis:             retirementBasis,
		Omissions:         knownOmissions,
	}

	// The savings-rate solve only means something with both ages known. It is
	// omitted rather than returned empty so the UI has nothing to render a blank
	// panel from.
	//
	// The current-age test reads the RESOLVED age rather than the stored column,
	// so a household that gave a birthdate instead of typing an age still gets
	// the solve. Testing stored.CurrentAge here would silently withhold it from
	// exactly the households that supplied the better input.
	if stored.TargetRetirementAge != nil && assumptions.CurrentAge > 0 {
		income, err := s.annualIncome(ctx, identity, now)
		if err != nil {
			s.internalError(w, "annual income", err)
			return
		}
		solve := networth.SolveRequiredSavings(plans, assumptions, income, now)
		resp.Savings = &solve
	}

	if s.Config.Retirement.MonteCarloEnabled {
		resp.MonteCarlo = s.runMonteCarlo(r, resp.Projection, assumptions)
	}

	writeJSON(w, http.StatusOK, resp)
}

// runMonteCarlo simulates the withdrawal phase from the nest egg at the target
// age. Nil when there is no target age, or nothing to draw down — a survival
// rate over an empty portfolio is not a figure worth printing.
func (s *Server) runMonteCarlo(
	r *http.Request,
	projection networth.RetirementProjection,
	a networth.RetirementAssumptions,
) *networth.MonteCarloResult {
	if projection.NestEggAtTarget == nil || !projection.NestEggAtTarget.IsPositive() {
		return nil
	}
	if !a.TargetAnnualSpending.IsPositive() {
		return nil
	}

	q := r.URL.Query()
	// Volatility has no honest default — it is an assumption, not a constant —
	// but the endpoint has to run without one, so the query parameter carries
	// it and the response's Basis names what was used either way.
	volatility := parseDecimal(q.Get("volatility"), decimal.RequireFromString("0.15"))

	// Social Security reduces what the portfolio itself has to cover, so the
	// withdrawal is the shortfall, not the whole spend. Ignoring it would model
	// a much harsher drawdown than the household will actually face.
	withdrawal := a.TargetAnnualSpending
	if a.AnnualSSIncome.IsPositive() && a.SSStartAge > 0 && a.TargetRetirementAge >= a.SSStartAge {
		withdrawal = withdrawal.Sub(a.AnnualSSIncome)
	}
	if !withdrawal.IsPositive() {
		return nil
	}

	result := networth.SimulateWithdrawals(networth.MonteCarloInput{
		StartBalance:     *projection.NestEggAtTarget,
		AnnualWithdrawal: withdrawal,
		MeanRealReturn:   a.RealReturnRate,
		Volatility:       volatility,
		Years:            parseInt(q.Get("years"), networth.DefaultRetirementYears, 5, 60),
		Runs:             networth.ClampRuns(parseInt(q.Get("runs"), networth.DefaultMonteCarloRuns, 0, 10000)),
	})
	return &result
}

// annualIncome is the trailing year's gross income, the denominator for the
// required savings RATE. Zero is a legitimate answer (a household with no
// categorised income yet), and the solve returns a nil rate rather than
// dividing by it.
func (s *Server) annualIncome(
	ctx context.Context, identity auth.Identity, now time.Time,
) (decimal.Decimal, error) {
	summary, err := s.Queries.GetSpendingSummary(ctx, dbgen.GetSpendingSummaryParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        now.AddDate(-1, 0, 0),
		Date_2:      now,
	})
	if err != nil {
		return decimal.Zero, err
	}
	return summary.Income.Round(2), nil
}

// defaultProjectionMonths runs to the target retirement age when one is set,
// plus a few years past it so the chart shows what happens after rather than
// stopping dead on the milestone. Thirty years otherwise.
func defaultProjectionMonths(a dbgen.ProjectionAssumption) int {
	if a.TargetRetirementAge != nil && a.CurrentAge != nil {
		years := int(*a.TargetRetirementAge) - int(*a.CurrentAge)
		if years > 0 {
			return (years + 5) * 12
		}
	}
	return 30 * 12
}

// toRetirementAssumptions maps the stored row onto the engine's input, applying
// the trailing-spend default when the user has not set a target.
func toRetirementAssumptions(
	row dbgen.ProjectionAssumption, defaultedSpending decimal.Decimal,
	birthdate *time.Time, now time.Time,
) networth.RetirementAssumptions {
	a := networth.RetirementAssumptions{
		RealReturnRate:       row.RealReturnRate,
		WithdrawalRate:       row.WithdrawalRate,
		TargetAnnualSpending: defaultedSpending,
	}
	if row.TargetAnnualSpending.Valid {
		a.TargetAnnualSpending = row.TargetAnnualSpending.Decimal
	}
	// Prefer the caller's birthdate; fall back to the stored age; leave zero
	// when there is neither, which is the existing "not decided" state the
	// engine already reports rather than substitutes for.
	stored := 0
	if row.CurrentAge != nil {
		stored = int(*row.CurrentAge)
	}
	if age, ok := networth.ResolveAge(birthdate, stored, now); ok {
		a.CurrentAge = age
	}
	if row.TargetRetirementAge != nil {
		a.TargetRetirementAge = int(*row.TargetRetirementAge)
	}
	if row.SsStartAge != nil {
		a.SSStartAge = int(*row.SsStartAge)
	}
	if row.AnnualSsIncome.Valid {
		a.AnnualSSIncome = row.AnnualSsIncome.Decimal
	}
	return a
}

// toAccountPlans maps query rows onto the engine's plans. A NULL tax_treatment
// becomes an empty Treatment, which the engine excludes and reports — the whole
// point of leaving the column nullable in doc 14.
func toAccountPlans(rows []dbgen.ListProjectableAccountsRow, now time.Time) []networth.AccountPlan {
	plans := make([]networth.AccountPlan, 0, len(rows))
	for _, r := range rows {
		p := networth.AccountPlan{ID: r.ID.String(), Name: r.Name}
		if r.TaxTreatment != nil {
			p.Treatment = *r.TaxTreatment
		}
		if r.CurrentBalance.Valid {
			p.Balance = r.CurrentBalance.Decimal
		}
		if r.MonthlyContribution.Valid {
			p.MonthlyContribution = r.MonthlyContribution.Decimal
		}
		if r.EmployerMatchPct.Valid {
			p.EmployerMatchPct = r.EmployerMatchPct.Decimal
		}
		if r.AnnualSalary.Valid {
			p.AnnualSalary = r.AnnualSalary.Decimal
		}
		if r.EmployerMatchLimit.Valid {
			p.EmployerMatchLimit = r.EmployerMatchLimit.Decimal
		}
		stored := 0
		if r.BeneficiaryCurrentAge != nil {
			stored = int(*r.BeneficiaryCurrentAge)
		}
		if age, ok := networth.ResolveAge(r.BeneficiaryBirthdate, stored, now); ok {
			p.BeneficiaryCurrentAge = age
		}
		if r.BeneficiaryTargetAge != nil {
			p.BeneficiaryTargetAge = int(*r.BeneficiaryTargetAge)
		}
		plans = append(plans, p)
	}
	return plans
}

// --------------------------------------------------------------------------
// Small parsing helpers
// --------------------------------------------------------------------------

// parseRate reads a rate expressed as a fraction ("0.05"), rejecting anything
// outside a sane band. A user who types "5" meaning 5% gets an error naming the
// expected form rather than a projection compounding at 500% a year.
func parseRate(raw, field string) (decimal.Decimal, error) {
	if raw == "" {
		return decimal.Zero, errors.New(field + " is required, as a fraction (0.05 = 5%)")
	}
	v, err := decimal.NewFromString(raw)
	if err != nil {
		return decimal.Zero, errors.New(field + " must be a decimal fraction, e.g. \"0.05\" for 5%")
	}
	if v.IsNegative() || v.GreaterThan(decimal.NewFromInt(maxRatePercent).Div(decimal.NewFromInt(100))) {
		return decimal.Zero, errors.New(field + " must be between 0 and 0.30 (that is 0%–30%, as a fraction)")
	}
	return v, nil
}

func optionalRate(raw *string, field string) (decimal.NullDecimal, error) {
	if raw == nil || *raw == "" {
		return decimal.NullDecimal{}, nil
	}
	v, err := parseRate(*raw, field)
	if err != nil {
		return decimal.NullDecimal{}, err
	}
	return decimal.NewNullDecimal(v), nil
}

func optionalMoney(raw *string, field string) (decimal.NullDecimal, error) {
	if raw == nil || *raw == "" {
		return decimal.NullDecimal{}, nil
	}
	v, err := decimal.NewFromString(*raw)
	if err != nil {
		return decimal.NullDecimal{}, errors.New(field + " must be a decimal number, e.g. \"24000.00\"")
	}
	if v.IsNegative() {
		return decimal.NullDecimal{}, errors.New(field + " cannot be negative")
	}
	return decimal.NewNullDecimal(v), nil
}

// optionalAge returns nil for an absent age — "not decided" is a real answer
// and must survive a round trip as null rather than collapsing to zero.
func optionalAge(raw *int, field string) (*int32, error) {
	if raw == nil {
		return nil, nil
	}
	if *raw < minAge || *raw > maxAge {
		return nil, errors.New(field + " must be between 1 and 120")
	}
	v := int32(*raw)
	return &v, nil
}
