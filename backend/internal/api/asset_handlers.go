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

// Asset revaluation, depreciation and directly-held bonds (doc 26).
//
// The organising rule of this file, and the one to preserve when changing it:
//
//	AN ESTIMATE NEVER WRITES A VALUE.
//
// handleAssetSuggestion computes a depreciation proposal and returns it. It
// does not touch manual_assets. The only paths that write a value are
// handleCreateValuation (the user accepted a figure or typed their own) and the
// bond revaluation job — and the bond job is licensed by determinism, not
// convenience: a savings bond's value is arithmetic over published rates, and
// the same inputs give the same answer to the cent every time. The moment a
// valuation depends on a judgement it goes back to being a proposal.

// --------------------------------------------------------------------------
// Class-specific detail
// --------------------------------------------------------------------------

type assetDetailResponse struct {
	Address *string          `json:"address"`
	Beds    *decimal.Decimal `json:"beds"`
	Baths   *decimal.Decimal `json:"baths"`
	Sqft    *int32           `json:"sqft"`
	LotSqft *int32           `json:"lot_sqft"`

	Year          *int32  `json:"year"`
	Make          *string `json:"make"`
	Model         *string `json:"model"`
	Trim          *string `json:"trim"`
	Mileage       *int32  `json:"mileage"`
	AnnualMileage *int32  `json:"annual_mileage"`

	BondSeries    *string          `json:"bond_series"`
	IssueDate     *string          `json:"issue_date"`
	PurchasePrice *decimal.Decimal `json:"purchase_price"`
	FaceValue     *decimal.Decimal `json:"face_value"`
	CouponRate    *decimal.Decimal `json:"coupon_rate"`
	MaturityDate  *string          `json:"maturity_date"`
	TaxExempt     *bool            `json:"tax_exempt"`

	Condition *string `json:"condition"`
}

func detailResponse(d dbgen.AssetDetail) assetDetailResponse {
	out := assetDetailResponse{
		Address: d.Address, Sqft: d.Sqft, LotSqft: d.LotSqft,
		Year: d.Year, Make: d.Make, Model: d.Model, Trim: d.Trim,
		Mileage: d.Mileage, AnnualMileage: d.AnnualMileage,
		BondSeries: d.BondSeries, TaxExempt: d.TaxExempt, Condition: d.Condition,
	}
	out.Beds = nullDecimalPtr(d.Beds)
	out.Baths = nullDecimalPtr(d.Baths)
	out.PurchasePrice = nullDecimalPtr(d.PurchasePrice)
	out.FaceValue = nullDecimalPtr(d.FaceValue)
	out.CouponRate = nullDecimalPtr(d.CouponRate)
	out.IssueDate = datePtr(d.IssueDate)
	out.MaturityDate = datePtr(d.MaturityDate)
	return out
}

func nullDecimalPtr(d decimal.NullDecimal) *decimal.Decimal {
	if !d.Valid {
		return nil
	}
	v := d.Decimal
	return &v
}

func datePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.DateOnly)
	return &s
}

func (s *Server) handleGetAssetDetail(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	assetID, ok := assetIDParam(w, r)
	if !ok {
		return
	}

	d, err := s.Queries.GetAssetDetail(r.Context(), dbgen.GetAssetDetailParams{
		ManualAssetID: assetID, HouseholdID: identity.HouseholdID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No detail recorded is the normal state for most assets, not an
			// error. An empty object lets the form render without a special case.
			writeJSON(w, http.StatusOK, assetDetailResponse{})
			return
		}
		s.internalError(w, "get asset detail", err)
		return
	}
	writeJSON(w, http.StatusOK, detailResponse(d))
}

type assetDetailRequest struct {
	Address *string `json:"address"`
	Beds    *string `json:"beds"`
	Baths   *string `json:"baths"`
	Sqft    *int32  `json:"sqft"`
	LotSqft *int32  `json:"lot_sqft"`

	Year          *int32  `json:"year"`
	Make          *string `json:"make"`
	Model         *string `json:"model"`
	Trim          *string `json:"trim"`
	Mileage       *int32  `json:"mileage"`
	AnnualMileage *int32  `json:"annual_mileage"`

	BondSeries *string `json:"bond_series"`
	IssueDate  *string `json:"issue_date"`
	// Money arrives as strings so it never passes through a JSON float.
	PurchasePrice *string `json:"purchase_price"`
	FaceValue     *string `json:"face_value"`
	CouponRate    *string `json:"coupon_rate"`
	MaturityDate  *string `json:"maturity_date"`
	TaxExempt     *bool   `json:"tax_exempt"`

	Condition *string `json:"condition"`
}

func (s *Server) handleUpsertAssetDetail(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	assetID, ok := assetIDParam(w, r)
	if !ok {
		return
	}
	// Confirms the asset is this household's before writing anything hanging
	// off it.
	if _, err := s.Queries.GetManualAsset(r.Context(), dbgen.GetManualAssetParams{
		ID: assetID, HouseholdID: identity.HouseholdID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "asset not found")
			return
		}
		s.internalError(w, "get manual asset", err)
		return
	}

	var req assetDetailRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	params := dbgen.UpsertAssetDetailParams{
		ManualAssetID: assetID,
		Address:       trimmed(req.Address),
		Sqft:          req.Sqft,
		LotSqft:       req.LotSqft,
		Year:          req.Year,
		Make:          trimmed(req.Make),
		Model:         trimmed(req.Model),
		Trim:          trimmed(req.Trim),
		Mileage:       req.Mileage,
		AnnualMileage: req.AnnualMileage,
		TaxExempt:     req.TaxExempt,
	}

	var bad string
	if params.Beds, bad = parseOptionalDecimal(req.Beds, 0, 100, "beds must be a number"); bad != "" {
		writeError(w, http.StatusBadRequest, bad)
		return
	}
	if params.Baths, bad = parseOptionalDecimal(req.Baths, 0, 100, "baths must be a number"); bad != "" {
		writeError(w, http.StatusBadRequest, bad)
		return
	}
	if params.PurchasePrice, bad = parseOptionalDecimal(req.PurchasePrice, 0, 0,
		"purchase price must be a decimal number, e.g. \"1000.00\""); bad != "" {
		writeError(w, http.StatusBadRequest, bad)
		return
	}
	if params.FaceValue, bad = parseOptionalDecimal(req.FaceValue, 0, 0,
		"face value must be a decimal number, e.g. \"1000.00\""); bad != "" {
		writeError(w, http.StatusBadRequest, bad)
		return
	}
	if params.CouponRate, bad = parseOptionalDecimal(req.CouponRate, 0, 100,
		"coupon rate must be a percentage, e.g. \"4.25\""); bad != "" {
		writeError(w, http.StatusBadRequest, bad)
		return
	}

	if req.BondSeries != nil && *req.BondSeries != "" {
		switch *req.BondSeries {
		case networth.SeriesISavings, networth.SeriesEESavings,
			networth.SeriesTreasury, networth.SeriesOther:
			params.BondSeries = req.BondSeries
		default:
			writeError(w, http.StatusBadRequest,
				"bond series must be one of i_savings, ee_savings, treasury, other")
			return
		}
	}
	if req.Condition != nil && *req.Condition != "" {
		switch *req.Condition {
		case "excellent", "good", "fair", "poor":
			params.Condition = req.Condition
		default:
			writeError(w, http.StatusBadRequest,
				"condition must be one of excellent, good, fair, poor")
			return
		}
	}

	var err error
	if params.IssueDate, err = optionalAssetDate(req.IssueDate); err != nil {
		writeError(w, http.StatusBadRequest, "issue date must be YYYY-MM-DD")
		return
	}
	if params.MaturityDate, err = optionalAssetDate(req.MaturityDate); err != nil {
		writeError(w, http.StatusBadRequest, "maturity date must be YYYY-MM-DD")
		return
	}

	d, err := s.Queries.UpsertAssetDetail(r.Context(), params)
	if err != nil {
		s.internalError(w, "upsert asset detail", err)
		return
	}
	writeJSON(w, http.StatusOK, detailResponse(d))
}

// --------------------------------------------------------------------------
// Valuation history
// --------------------------------------------------------------------------

type valuationResponse struct {
	Value  decimal.Decimal `json:"value"`
	AsOf   string          `json:"as_of"`
	Source string          `json:"source"`
	Note   *string         `json:"note"`
}

func (s *Server) handleListValuations(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	assetID, ok := assetIDParam(w, r)
	if !ok {
		return
	}

	rows, err := s.Queries.ListAssetValuations(r.Context(), dbgen.ListAssetValuationsParams{
		ManualAssetID: assetID, HouseholdID: identity.HouseholdID,
	})
	if err != nil {
		s.internalError(w, "list asset valuations", err)
		return
	}

	out := make([]valuationResponse, 0, len(rows))
	for _, v := range rows {
		out = append(out, valuationResponse{
			Value: v.Value, AsOf: v.AsOf.Format(time.DateOnly), Source: v.Source, Note: v.Note,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type createValuationRequest struct {
	Value string  `json:"value"`
	AsOf  *string `json:"as_of"`
	// Source is 'manual' when the user typed a figure and 'estimated' when they
	// accepted one the app proposed. Both are user decisions; the distinction
	// is recorded so the history can show which is which.
	Source string  `json:"source"`
	Note   *string `json:"note"`
}

// handleCreateValuation records a revaluation.
//
// Both writes happen in one transaction. manual_assets.value is the current
// value every net-worth query reads, and asset_valuations is the history behind
// it — a failure that left one without the other would make the headline figure
// and its own explanation disagree.
func (s *Server) handleCreateValuation(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()

	assetID, ok := assetIDParam(w, r)
	if !ok {
		return
	}

	var req createValuationRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	value, err := decimal.NewFromString(req.Value)
	if err != nil {
		writeError(w, http.StatusBadRequest, "value must be a decimal number, e.g. \"425000.00\"")
		return
	}
	switch req.Source {
	case "":
		req.Source = "manual"
	case "manual", "estimated", "api":
	default:
		writeError(w, http.StatusBadRequest, "source must be one of manual, estimated, api")
		return
	}

	asOf := time.Now().UTC().Truncate(24 * time.Hour)
	if req.AsOf != nil && *req.AsOf != "" {
		parsed, err := time.Parse(time.DateOnly, *req.AsOf)
		if err != nil {
			writeError(w, http.StatusBadRequest, "as_of must be YYYY-MM-DD")
			return
		}
		asOf = parsed
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		s.internalError(w, "begin valuation", err)
		return
	}
	defer tx.Rollback(ctx)
	qtx := s.Queries.WithTx(tx)

	if _, err := qtx.InsertAssetValuation(ctx, dbgen.InsertAssetValuationParams{
		ManualAssetID: assetID, Value: value, AsOf: asOf, Source: req.Source, Note: req.Note,
	}); err != nil {
		s.internalError(w, "insert asset valuation", err)
		return
	}

	asset, err := qtx.SetManualAssetValue(ctx, dbgen.SetManualAssetValueParams{
		ID: assetID, HouseholdID: identity.HouseholdID, Value: value, AsOf: asOf,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "asset not found")
			return
		}
		s.internalError(w, "set manual asset value", err)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		s.internalError(w, "commit valuation", err)
		return
	}

	writeJSON(w, http.StatusCreated, manualAssetResponse{
		ID: asset.ID, Name: asset.Name, Kind: asset.Kind, Value: asset.Value,
		IsLiability: asset.IsLiability, AsOf: asset.AsOf.Format(time.DateOnly), Notes: asset.Notes,
	})
}

// --------------------------------------------------------------------------
// Proposals — computed, never written
// --------------------------------------------------------------------------

type suggestionResponse struct {
	// OK false means no proposal could be made, and Reason says why. The UI
	// shows the reason rather than a number.
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`

	Value   *decimal.Decimal `json:"value,omitempty"`
	Change  *decimal.Decimal `json:"change,omitempty"`
	Current decimal.Decimal  `json:"current"`
	Basis   string           `json:"basis,omitempty"`

	// Estimate is always true, and it is in the payload so a client cannot
	// render the figure without the caveat travelling with it — the same
	// treatment the projection endpoint gives its numbers.
	Estimate bool `json:"estimate"`
}

// handleAssetSuggestion proposes a new value for a depreciating asset.
//
// It writes NOTHING. The response is a proposal the user accepts (by POSTing a
// valuation) or ignores. Net worth must not move on a guess.
func (s *Server) handleAssetSuggestion(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()

	assetID, ok := assetIDParam(w, r)
	if !ok {
		return
	}

	asset, err := s.Queries.GetManualAsset(ctx, dbgen.GetManualAssetParams{
		ID: assetID, HouseholdID: identity.HouseholdID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "asset not found")
			return
		}
		s.internalError(w, "get manual asset", err)
		return
	}

	out := suggestionResponse{Current: asset.Value, Estimate: true}

	if asset.Kind != "vehicle" {
		out.Reason = "only vehicles have a depreciation curve. Enter a value for anything else — " +
			"a home's worth is a market question this app has no honest source for."
		writeJSON(w, http.StatusOK, out)
		return
	}

	detail, err := s.Queries.GetAssetDetail(ctx, dbgen.GetAssetDetailParams{
		ManualAssetID: assetID, HouseholdID: identity.HouseholdID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			out.Reason = "add the model year to estimate depreciation"
			writeJSON(w, http.StatusOK, out)
			return
		}
		s.internalError(w, "get asset detail", err)
		return
	}

	in, ok := networth.VehicleFromDetail(asset.Value, asset.AsOf, detail)
	if !ok {
		out.Reason = "add the model year to estimate depreciation"
		writeJSON(w, http.StatusOK, out)
		return
	}

	sug := networth.SuggestVehicleValue(in, time.Now().UTC())
	out.OK, out.Reason, out.Basis = sug.OK, sug.Reason, sug.Basis
	if sug.OK {
		value, change := sug.Value, sug.Change
		out.Value, out.Change = &value, &change
	}
	writeJSON(w, http.StatusOK, out)
}

// --------------------------------------------------------------------------
// Bond valuation
// --------------------------------------------------------------------------

type appliedRateResponse struct {
	PeriodStart string           `json:"period_start"`
	Announced   string           `json:"announced"`
	Fixed       decimal.Decimal  `json:"fixed_rate"`
	Inflation   *decimal.Decimal `json:"inflation_rate"`
	Composite   decimal.Decimal  `json:"composite_rate"`
	Months      int              `json:"months"`
}

type bondValueResponse struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`

	// Redemption is what the bond could be turned into today and the only
	// figure that enters net worth. Accrued is what it has earned; the two
	// differ inside the first five years and both are shown, so the forfeited
	// interest is visible rather than swallowed.
	Redemption decimal.Decimal `json:"redemption_value"`
	Accrued    decimal.Decimal `json:"accrued_value"`

	PenaltyApplied  bool `json:"penalty_applied"`
	DoublingApplied bool `json:"doubling_applied"`
	Matured         bool `json:"matured"`

	AsOf          string `json:"as_of"`
	ValuedThrough string `json:"valued_through"`
	FinalMaturity string `json:"final_maturity,omitempty"`
	Basis         string `json:"basis,omitempty"`

	// MonthsToDoubling counts down an EE bond's 20-year guarantee. Nil for
	// anything else, or once it has passed.
	MonthsToDoubling *int `json:"months_to_doubling,omitempty"`

	Rates []appliedRateResponse `json:"rates"`
}

func (s *Server) handleBondValue(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()

	assetID, ok := assetIDParam(w, r)
	if !ok {
		return
	}

	detail, err := s.Queries.GetAssetDetail(ctx, dbgen.GetAssetDetailParams{
		ManualAssetID: assetID, HouseholdID: identity.HouseholdID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "no bond details recorded for this asset")
			return
		}
		s.internalError(w, "get asset detail", err)
		return
	}

	bond, ok := networth.BondFromDetail(detail)
	if !ok {
		writeError(w, http.StatusBadRequest,
			"this asset needs a series, an issue date and a purchase price before it can be valued")
		return
	}

	rates, err := networth.LoadBondRates(ctx, s.Queries)
	if err != nil {
		s.internalError(w, "load savings bond rates", err)
		return
	}

	writeJSON(w, http.StatusOK, bondResponse(networth.ValueBond(bond, rates, time.Now().UTC()), bond))
}

func bondResponse(v networth.BondValue, b networth.Bond) bondValueResponse {
	out := bondValueResponse{
		OK: v.OK, Reason: v.Reason,
		Redemption: v.Redemption, Accrued: v.Accrued,
		PenaltyApplied: v.PenaltyApplied, DoublingApplied: v.DoublingApplied,
		Matured: v.Matured,
		AsOf:    v.AsOf.Format(time.DateOnly),
		Basis:   v.Basis,
		Rates:   make([]appliedRateResponse, 0, len(v.Rates)),
	}
	if !v.ValuedThrough.IsZero() {
		out.ValuedThrough = v.ValuedThrough.Format(time.DateOnly)
	}
	if !v.FinalMaturity.IsZero() {
		out.FinalMaturity = v.FinalMaturity.Format(time.DateOnly)
	}

	// How far an EE bond is from its doubling, which is the single most
	// useful thing to know about one.
	if b.Series == networth.SeriesEESavings && !v.DoublingApplied {
		months := networth.MonthsToDoubling(b.IssueDate, v.AsOf)
		if months > 0 {
			out.MonthsToDoubling = &months
		}
	}

	for _, r := range v.Rates {
		out.Rates = append(out.Rates, appliedRateResponse{
			PeriodStart: r.PeriodStart.Format(time.DateOnly),
			Announced:   r.Announced.Format(time.DateOnly),
			Fixed:       r.Fixed,
			Inflation:   nullDecimalPtr(r.Inflation),
			Composite:   r.Composite,
			Months:      r.Months,
		})
	}
	return out
}

// --------------------------------------------------------------------------
// Linking a loan
// --------------------------------------------------------------------------

type linkLoanRequest struct {
	// Null or empty unlinks.
	LoanAccountID *string `json:"loan_account_id"`
}

func (s *Server) handleLinkAssetLoan(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()

	assetID, ok := assetIDParam(w, r)
	if !ok {
		return
	}

	var req linkLoanRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	params := dbgen.SetManualAssetLoanParams{ID: assetID, HouseholdID: identity.HouseholdID}
	if req.LoanAccountID != nil && *req.LoanAccountID != "" {
		accountID, err := uuid.Parse(*req.LoanAccountID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid loan account id")
			return
		}
		// The account has to be a debt this household can actually see. Same
		// visibility rule as everywhere else, and the same deliberately vague
		// 404 so nothing leaks about which ids are real.
		if _, err := s.Queries.GetVisibleLiability(ctx, dbgen.GetVisibleLiabilityParams{
			HouseholdID: identity.HouseholdID, UserID: identity.UserID, ID: accountID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "loan account not found")
				return
			}
			s.internalError(w, "get visible liability", err)
			return
		}
		params.LoanAccountID = &accountID
	}

	asset, err := s.Queries.SetManualAssetLoan(ctx, params)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "asset not found")
			return
		}
		s.internalError(w, "link asset loan", err)
		return
	}

	writeJSON(w, http.StatusOK, manualAssetResponse{
		ID: asset.ID, Name: asset.Name, Kind: asset.Kind, Value: asset.Value,
		IsLiability: asset.IsLiability, AsOf: asset.AsOf.Format(time.DateOnly), Notes: asset.Notes,
	})
}

// --------------------------------------------------------------------------
// The published rate table
// --------------------------------------------------------------------------

type bondRateResponse struct {
	Series      string           `json:"series"`
	PeriodStart string           `json:"period_start"`
	Fixed       decimal.Decimal  `json:"fixed_rate"`
	Inflation   *decimal.Decimal `json:"inflation_rate"`
	SourceURL   string           `json:"source_url"`
}

// handleListBondRates exposes the seeded table.
//
// It is readable and editable on purpose. A bundled table of numbers is only
// defensible if the user can check it, which means seeing every row with the
// URL it came from and being able to correct one.
func (s *Server) handleListBondRates(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Queries.ListSavingsBondRates(r.Context())
	if err != nil {
		s.internalError(w, "list savings bond rates", err)
		return
	}

	out := make([]bondRateResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, bondRateResponse{
			Series: row.Series, PeriodStart: row.PeriodStart.Format(time.DateOnly),
			Fixed: row.FixedRate, Inflation: nullDecimalPtr(row.InflationRate),
			SourceURL: row.SourceUrl,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type upsertBondRateRequest struct {
	Series      string  `json:"series"`
	PeriodStart string  `json:"period_start"`
	Fixed       string  `json:"fixed_rate"`
	Inflation   *string `json:"inflation_rate"`
	SourceURL   string  `json:"source_url"`
}

func (s *Server) handleUpsertBondRate(w http.ResponseWriter, r *http.Request) {
	var req upsertBondRateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.Series != networth.SeriesISavings && req.Series != networth.SeriesEESavings {
		writeError(w, http.StatusBadRequest, "series must be i_savings or ee_savings")
		return
	}
	periodStart, err := time.Parse(time.DateOnly, req.PeriodStart)
	if err != nil {
		writeError(w, http.StatusBadRequest, "period start must be YYYY-MM-DD")
		return
	}
	fixed, err := decimal.NewFromString(req.Fixed)
	if err != nil {
		writeError(w, http.StatusBadRequest, "fixed rate must be a percentage, e.g. \"1.30\"")
		return
	}
	// Provenance is the entire justification for shipping this table. A row
	// without a source is the "transcribed numbers nobody can verify" problem
	// the seed was designed to avoid, so it is refused rather than stored.
	if req.SourceURL == "" {
		writeError(w, http.StatusBadRequest,
			"a source URL is required, so any figure this produces can be checked against treasurydirect.gov")
		return
	}

	inflation, bad := parseOptionalDecimal(req.Inflation, -100, 100,
		"inflation rate must be a percentage, e.g. \"1.56\"")
	if bad != "" {
		writeError(w, http.StatusBadRequest, bad)
		return
	}
	if req.Series == networth.SeriesISavings && !inflation.Valid {
		writeError(w, http.StatusBadRequest, "an I bond period needs an inflation rate")
		return
	}
	if req.Series == networth.SeriesEESavings && inflation.Valid {
		writeError(w, http.StatusBadRequest, "an EE bond period has no inflation rate")
		return
	}

	row, err := s.Queries.UpsertSavingsBondRate(r.Context(), dbgen.UpsertSavingsBondRateParams{
		Series: req.Series, PeriodStart: periodStart, FixedRate: fixed,
		InflationRate: inflation, SourceUrl: req.SourceURL,
	})
	if err != nil {
		s.internalError(w, "upsert savings bond rate", err)
		return
	}

	writeJSON(w, http.StatusOK, bondRateResponse{
		Series: row.Series, PeriodStart: row.PeriodStart.Format(time.DateOnly),
		Fixed: row.FixedRate, Inflation: nullDecimalPtr(row.InflationRate),
		SourceURL: row.SourceUrl,
	})
}

// --------------------------------------------------------------------------
// Shared helpers
// --------------------------------------------------------------------------

func assetIDParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "assetID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid asset id")
		return uuid.Nil, false
	}
	return id, true
}

// trimmed turns an empty string into a NULL, so clearing a field in the form
// stores nothing rather than an empty string that reads as "recorded".
func trimmed(s *string) *string {
	if s == nil || *s == "" {
		return nil
	}
	return s
}

func optionalAssetDate(raw *string) (*time.Time, error) {
	if raw == nil || *raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.DateOnly, *raw)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
