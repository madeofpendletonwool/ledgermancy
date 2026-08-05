package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/payroll"
)

// The derived payroll surface: the year overview, contribution headroom, the
// gross-based savings rate and the annual tax summary.
//
// Every figure here is computed from CONFIRMED stubs only. That is enforced in
// SQL — ListConfirmedPaystubsForYear and GetConfirmedGrossForPeriod both filter
// on confirmed_at — so no handler in this file has to remember to, and an
// unconfirmed stub is inert by construction rather than by convention.

// --------------------------------------------------------------------------
// Taxonomy
// --------------------------------------------------------------------------

type payrollTaxonomyResponse struct {
	Categories []payroll.Descriptor `json:"categories"`
	// PayFrequencies carry their periods-per-year so the client can label the
	// cadence without a second table that can disagree with the Go one.
	PayFrequencies []payFrequencyResponse `json:"pay_frequencies"`
}

type payFrequencyResponse struct {
	Value          string `json:"value"`
	Label          string `json:"label"`
	PeriodsPerYear int    `json:"periods_per_year"`
}

// handlePayrollTaxonomy serves the manual-entry form's schema.
//
// The taxonomy is a server fact — it has to match a CHECK constraint and the
// wage-base rules — so it is served rather than duplicated in TypeScript. A
// second copy in the frontend would be one deploy away from disagreeing about
// whether an HSA deduction reduces FICA wages.
func (s *Server) handlePayrollTaxonomy(w http.ResponseWriter, r *http.Request) {
	out := payrollTaxonomyResponse{Categories: payroll.Taxonomy()}
	for _, f := range []struct {
		value payroll.PayFrequency
		label string
	}{
		{payroll.Weekly, "Weekly"},
		{payroll.Biweekly, "Every two weeks"},
		{payroll.Semimonthly, "Twice a month"},
		{payroll.Monthly, "Monthly"},
	} {
		periods, _ := payroll.PeriodsPerYear(f.value)
		out.PayFrequencies = append(out.PayFrequencies, payFrequencyResponse{
			Value: string(f.value), Label: f.label, PeriodsPerYear: periods,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// --------------------------------------------------------------------------
// Years
// --------------------------------------------------------------------------

func (s *Server) handleListPaystubYears(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	years, err := s.Queries.ListPaystubYears(r.Context(), dbgen.ListPaystubYearsParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID,
	})
	if err != nil {
		s.internalError(w, "list paystub years", err)
		return
	}
	if years == nil {
		years = []int32{}
	}
	writeJSON(w, http.StatusOK, years)
}

// --------------------------------------------------------------------------
// The year overview
// --------------------------------------------------------------------------

type payrollEmployerSummary struct {
	EmployerID   uuid.UUID `json:"employer_id"`
	Name         string    `json:"name"`
	PayFrequency string    `json:"pay_frequency"`
	PaystubCount int       `json:"paystub_count"`
	Gross        string    `json:"gross"`
	Net          string    `json:"net"`
	LastPayDate  *string   `json:"last_pay_date"`
}

type headroomResponse struct {
	Group       string  `json:"group"`
	Label       string  `json:"label"`
	Contributed string  `json:"contributed"`
	Limit       string  `json:"limit"`
	Remaining   string  `json:"remaining"`
	OverBy      string  `json:"over_by"`
	PeriodsLeft *int    `json:"periods_left"`
	PerPeriod   *string `json:"per_period"`
}

type categoryTotalResponse struct {
	Category   string `json:"category"`
	Label      string `json:"label"`
	Group      string `json:"group"`
	Amount     string `json:"amount"`
	IsTax      bool   `json:"is_tax"`
	IsEmployer bool   `json:"is_employer"`
}

type payrollSummaryResponse struct {
	TaxYear int `json:"tax_year"`
	// HasData is false for a year with no confirmed stubs. The client shows an
	// empty state rather than a page of zeroes, which would read as "you earned
	// nothing" instead of "nothing has been entered".
	HasData      bool `json:"has_data"`
	PaystubCount int  `json:"paystub_count"`
	// UnconfirmedCount is the caller's own review queue.
	UnconfirmedCount int64 `json:"unconfirmed_count"`

	Gross             string           `json:"gross"`
	Net               string           `json:"net"`
	TaxTotal          string           `json:"tax_total"`
	EffectiveTaxRate  *decimal.Decimal `json:"effective_tax_rate"`
	EmployerTotal     string           `json:"employer_total"`
	TotalCompensation string           `json:"total_compensation"`

	Employers  []payrollEmployerSummary `json:"employers"`
	Categories []categoryTotalResponse  `json:"categories"`

	Headroom []headroomResponse `json:"headroom"`
	// LimitsConfigured is false when the tax year is not in networth's limits
	// table. Surfaced rather than silently substituting an adjacent year — the
	// rule networth/limits.go states, honoured by its second consumer.
	LimitsConfigured bool `json:"limits_configured"`
	LatestLimitYear  int  `json:"latest_limit_year"`
	// AgeKnown is false when neither a birthdate nor a stored age is available,
	// in which case the headroom above is the base limit with no catch-up. The
	// client says so; a 55-year-old must not be told they have less room than
	// they do without being told why.
	AgeKnown bool `json:"age_known"`
}

func (s *Server) handlePayrollSummary(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	now := time.Now().UTC()

	taxYear, ok := yearParam(w, r, now.Year())
	if !ok {
		return
	}

	year, stubCount, err := s.buildPayrollYear(r, taxYear)
	if err != nil {
		s.internalError(w, "assemble payroll year", err)
		return
	}

	unconfirmed, err := s.Queries.CountUnconfirmedPaystubs(r.Context(), dbgen.CountUnconfirmedPaystubsParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID,
	})
	if err != nil {
		s.internalError(w, "count unconfirmed paystubs", err)
		return
	}

	age, ageKnown := s.resolveCallerAge(r, now)
	familyHSA := r.URL.Query().Get("family_hsa") == "true"

	out := payrollSummaryResponse{
		TaxYear:           taxYear,
		HasData:           stubCount > 0,
		PaystubCount:      stubCount,
		UnconfirmedCount:  unconfirmed,
		Gross:             year.Gross().StringFixed(2),
		Net:               year.Net().StringFixed(2),
		TaxTotal:          year.TaxTotal().StringFixed(2),
		EmployerTotal:     year.EmployerTotal().StringFixed(2),
		TotalCompensation: year.TotalCompensation().StringFixed(2),
		LatestLimitYear:   networth.LatestConfiguredYear(),
		AgeKnown:          ageKnown,
		Employers:         make([]payrollEmployerSummary, 0, len(year.Employers)),
		Categories:        make([]categoryTotalResponse, 0),
		Headroom:          make([]headroomResponse, 0),
	}
	if rate, ok := year.EffectiveTaxRate(); ok {
		rounded := rate.Round(4)
		out.EffectiveTaxRate = &rounded
	}

	for _, e := range year.Employers {
		summary := payrollEmployerSummary{
			Name:         e.Name,
			PayFrequency: string(e.Frequency),
			PaystubCount: len(e.Stubs),
			Gross:        e.Gross().StringFixed(2),
			Net:          e.Net().StringFixed(2),
		}
		if id, err := uuid.Parse(e.Key); err == nil {
			summary.EmployerID = id
		}
		if last, ok := e.LastPayDate(); ok {
			formatted := last.Format(time.DateOnly)
			summary.LastPayDate = &formatted
		}
		out.Employers = append(out.Employers, summary)
	}

	for _, c := range payroll.Categories() {
		total := year.CategoryTotal(c)
		if total.IsZero() {
			continue
		}
		spec := payroll.SpecFor(c)
		out.Categories = append(out.Categories, categoryTotalResponse{
			Category: string(c), Label: spec.Label, Group: spec.Group,
			Amount: total.StringFixed(2),
			IsTax:  spec.IsTax, IsEmployer: spec.EmployerOnly,
		})
	}

	headroom, configured := year.ContributionHeadroom(age, familyHSA)
	out.LimitsConfigured = configured
	for _, h := range headroom {
		item := headroomResponse{
			Group: h.Group, Label: h.Label,
			Contributed: h.Contributed.StringFixed(2),
			Limit:       h.Limit.StringFixed(2),
			Remaining:   h.Remaining.StringFixed(2),
			OverBy:      h.OverBy.StringFixed(2),
			PeriodsLeft: h.PeriodsLeft,
		}
		if h.PerPeriod != nil {
			perPeriod := h.PerPeriod.StringFixed(2)
			item.PerPeriod = &perPeriod
		}
		out.Headroom = append(out.Headroom, item)
	}

	writeJSON(w, http.StatusOK, out)
}

// --------------------------------------------------------------------------
// The gross-based savings rate
// --------------------------------------------------------------------------

type grossSavingsResponse struct {
	From string `json:"from"`
	To   string `json:"to"`
	// NetIncome is the ledger's income figure: what actually landed in the
	// household's accounts. Named explicitly because the whole point of this
	// endpoint is that it is NOT the same thing as gross pay.
	NetIncome string `json:"net_income"`
	Spending  string `json:"spending"`
	Leftover  string `json:"leftover"`
	// SavingsRateNet is the existing figure, unchanged: leftover as a share of
	// income that reached the accounts. It is what the recap, the insight feed
	// and the assistant already quote, and redefining it here would move all
	// three without a single call site changing.
	SavingsRateNet *decimal.Decimal `json:"savings_rate_net"`

	// GrossPay is the confirmed paystub gross over the same window, and null
	// when no confirmed stub covers it.
	GrossPay *string `json:"gross_pay"`
	// SavingsRateGross is leftover as a share of GROSS pay — the honest figure,
	// and usually a good deal smaller than the net one. Null whenever GrossPay
	// is, because a savings rate against an unknown denominator is not a number
	// to show somebody with a caveat next to it.
	SavingsRateGross *decimal.Decimal `json:"savings_rate_gross"`
	PaystubCount     int64            `json:"paystub_count"`
	// Coverage warns when the window is only partly covered by paystubs, which
	// makes the gross rate optimistic. Empty when there is nothing to say.
	Coverage string `json:"coverage"`
}

func (s *Server) handlePayrollSavingsRate(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	from, to := period(r)

	ledger, err := s.Queries.GetSpendingSummary(r.Context(), dbgen.GetSpendingSummaryParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID,
		Date: from, Date_2: to,
	})
	if err != nil {
		s.internalError(w, "spending summary", err)
		return
	}
	payrollTotals, err := s.Queries.GetConfirmedGrossForPeriod(r.Context(), dbgen.GetConfirmedGrossForPeriodParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID,
		FromDate: from, ToDate: to,
	})
	if err != nil {
		s.internalError(w, "confirmed gross for period", err)
		return
	}

	leftover := ledger.Income.Sub(ledger.Spending)
	out := grossSavingsResponse{
		From:         from.Format(time.DateOnly),
		To:           to.Format(time.DateOnly),
		NetIncome:    ledger.Income.StringFixed(2),
		Spending:     ledger.Spending.StringFixed(2),
		Leftover:     leftover.StringFixed(2),
		PaystubCount: payrollTotals.PaystubCount,
	}
	if ledger.Income.IsPositive() {
		rate := leftover.Div(ledger.Income).Round(4)
		out.SavingsRateNet = &rate
	}

	if payrollTotals.PaystubCount > 0 && payrollTotals.Gross.IsPositive() {
		gross := payrollTotals.Gross.StringFixed(2)
		out.GrossPay = &gross
		rate := leftover.Div(payrollTotals.Gross).Round(4)
		out.SavingsRateGross = &rate

		// The honest caveat. A household that recorded two of four stubs in a
		// month has a gross figure that is half the truth, which makes the gross
		// savings rate look about twice as good as it is — the exact direction
		// this whole feature exists to correct. Detected by comparing the net
		// pay ON the stubs against the income the ledger actually saw.
		if ledger.Income.IsPositive() && payrollTotals.Net.LessThan(ledger.Income.Mul(decimal.NewFromFloat(0.9))) {
			out.Coverage = "The paystubs on file account for less of this period's income " +
				"than the ledger saw, so the gross figure is incomplete and the rate " +
				"against it is optimistic."
		}
	}

	writeJSON(w, http.StatusOK, out)
}

// --------------------------------------------------------------------------
// The annual tax summary
// --------------------------------------------------------------------------

type w2BoxResponse struct {
	Box    string `json:"box"`
	Code   string `json:"code"`
	Label  string `json:"label"`
	Amount string `json:"amount"`
}

type w2SummaryResponse struct {
	EmployerID   uuid.UUID `json:"employer_id"`
	EmployerName string    `json:"employer_name"`
	Address      *string   `json:"address"`
	// EIN is returned here and nowhere else. This is the packet that goes to an
	// accountant, so it is the one place the full number is genuinely needed.
	EIN   *string         `json:"ein"`
	Boxes []w2BoxResponse `json:"boxes"`
}

type taxSummaryResponse struct {
	TaxYear   int                 `json:"tax_year"`
	Employers []w2SummaryResponse `json:"employers"`
	// Disclaimer travels with the DATA, not only with the UI. This gets printed
	// and emailed away from the screen that framed it, and a page of W-2 box
	// numbers with no caveat on it looks exactly like a W-2.
	Disclaimer string `json:"disclaimer"`
}

func (s *Server) handlePayrollTaxSummary(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	taxYear, ok := yearParam(w, r, time.Now().UTC().Year())
	if !ok {
		return
	}

	year, _, err := s.buildPayrollYear(r, taxYear)
	if err != nil {
		s.internalError(w, "assemble payroll year", err)
		return
	}

	out := taxSummaryResponse{
		TaxYear:    taxYear,
		Disclaimer: payroll.W2Disclaimer,
		Employers:  make([]w2SummaryResponse, 0, len(year.Employers)),
	}

	for _, e := range year.Employers {
		summary := e.W2(taxYear)
		item := w2SummaryResponse{
			EmployerName: summary.EmployerName,
			Boxes:        make([]w2BoxResponse, 0, len(summary.Boxes)),
		}
		if id, err := uuid.Parse(e.Key); err == nil {
			item.EmployerID = id
			// Re-read the employer for the identifying fields the roll-up does
			// not carry. A failure is not fatal: a tax packet without an EIN is
			// still useful, and refusing the whole summary over one unreadable
			// sealed column would not be.
			if row, err := s.Queries.GetEmployer(r.Context(), dbgen.GetEmployerParams{
				ID: id, HouseholdID: identity.HouseholdID,
			}); err == nil {
				item.Address = row.Address
				item.EIN = s.openEIN(row.EinEncrypted)
			}
		}
		for _, b := range summary.Boxes {
			item.Boxes = append(item.Boxes, w2BoxResponse{
				Box: b.Box, Code: b.Code, Label: b.Label,
				Amount: b.Amount.StringFixed(2),
			})
		}
		out.Employers = append(out.Employers, item)
	}

	writeJSON(w, http.StatusOK, out)
}

// --------------------------------------------------------------------------
// Shared assembly
// --------------------------------------------------------------------------

// buildPayrollYear assembles the confirmed stubs for one tax year into the
// domain shape the roll-ups work on. The second result is the stub count, so a
// caller can distinguish "no data" from "zero".
func (s *Server) buildPayrollYear(r *http.Request, taxYear int) (payroll.Year, int, error) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListConfirmedPaystubsForYear(r.Context(), dbgen.ListConfirmedPaystubsForYearParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		TaxYear:     int32(taxYear),
	})
	if err != nil {
		return payroll.Year{}, 0, err
	}

	ids := make([]uuid.UUID, 0, len(rows))
	for _, p := range rows {
		ids = append(ids, p.ID)
	}
	linesByStub, err := s.paystubLines(r, ids)
	if err != nil {
		return payroll.Year{}, 0, err
	}

	// Grouped by employer, in the order the query returned them, so a year's
	// employers appear in a stable order rather than a map's.
	year := payroll.Year{TaxYear: taxYear}
	index := map[uuid.UUID]int{}
	for _, p := range rows {
		pos, seen := index[p.EmployerID]
		if !seen {
			year.Employers = append(year.Employers, payroll.EmployerYear{
				Key:       p.EmployerID.String(),
				Name:      p.EmployerName,
				Frequency: payroll.PayFrequency(p.PayFrequency),
			})
			pos = len(year.Employers) - 1
			index[p.EmployerID] = pos
		}
		year.Employers[pos].Stubs = append(year.Employers[pos].Stubs, payroll.DatedStub{
			Stub: payroll.Stub{
				Gross: p.Gross, Net: p.Net,
				Lines: toDomainLines(linesByStub[p.ID]),
			},
			PayDate:  p.PayDate,
			YTDGross: p.YtdGross,
			YTDNet:   p.YtdNet,
		})
	}
	return year, len(rows), nil
}

// resolveCallerAge resolves the caller's age for catch-up eligibility, using
// the same order the retirement projection uses: the linked person's birthdate
// first, the deprecated stored integer second, neither third.
//
// The false result is passed through to the client rather than defaulted. Doc
// 23's headroom figure is the kind of number somebody acts on, and quietly
// applying no catch-up to a 55-year-old understates their room by $8,000
// without saying so.
func (s *Server) resolveCallerAge(r *http.Request, now time.Time) (int, bool) {
	identity := auth.MustFromContext(r.Context())

	var birthdate *time.Time
	if person, err := s.Queries.GetPersonByUserID(r.Context(), &identity.UserID); err == nil {
		birthdate = person.Birthdate
	} else if !errors.Is(err, pgx.ErrNoRows) {
		// Not fatal: the stored integer below may still answer, and a headroom
		// figure without catch-up is better than no summary at all.
		return 0, false
	}

	stored := 0
	if assumptions, err := s.Queries.GetProjectionAssumptions(r.Context(), identity.HouseholdID); err == nil {
		if assumptions.CurrentAge != nil {
			stored = int(*assumptions.CurrentAge)
		}
	}
	return networth.ResolveAge(birthdate, stored, now)
}

// yearParam reads ?year=YYYY with a caller-supplied default.
func yearParam(w http.ResponseWriter, r *http.Request, fallback int) (int, bool) {
	year, byYear, ok := optionalYearParam(w, r)
	if !ok {
		return 0, false
	}
	if !byYear {
		return fallback, true
	}
	return int(year), true
}
