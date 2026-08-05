package api

import (
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
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/payroll"
)

// The payroll HTTP surface: employers, paystubs, and the deposit match.
//
// Three rules this file enforces that no other handler file has to:
//
//  1. READ ACCESS IS NOT WRITE ACCESS. A shared stub is visible to the
//     household; it is editable only by the person whose pay it is. Every
//     mutation resolves its row through GetOwnedPaystub, which filters on
//     user_id, rather than through the shared-visibility GetPaystub.
//  2. CONFIRMATION IS GATED ON THE STUB BALANCING. gross − Σ(employee
//     deductions) = net, within a cent. A stub that does not reconcile is a
//     mis-entry, and doc 23 is explicit that storing one silently poisons every
//     downstream figure. It can be saved as a draft; it cannot be confirmed.
//  3. THE DEPOSIT MATCH ONLY EVER PROPOSES. Nothing links a stub to a
//     transaction except a request in which a human named that transaction.

// maxDepositMatches bounds the candidate list. Ten is already more than a
// person will read; the ranking puts the right one first or the feature has
// failed anyway.
const maxDepositMatches = 10

// depositMatchWindowDays is how far either side of the pay date to look for the
// deposit.
//
// Five, and it is asymmetric in practice rather than in code: a direct deposit
// posts on or a day either side of the pay date, and the window is wide enough
// to absorb a bank holiday weekend without becoming wide enough to sweep in the
// previous period's paycheque on a weekly schedule.
const depositMatchWindowDays = 5

// --------------------------------------------------------------------------
// Employers
// --------------------------------------------------------------------------

type employerResponse struct {
	ID           uuid.UUID `json:"id"`
	Name         string    `json:"name"`
	Address      *string   `json:"address"`
	PayFrequency string    `json:"pay_frequency"`
	// PaystubCount is scoped to what the caller can see, so it never reports
	// another member's private stubs.
	PaystubCount int64 `json:"paystub_count"`
	// HasEIN and EINMasked, never the EIN itself. The full value is returned by
	// exactly one endpoint — the annual tax summary, which is the only place it
	// is needed — so a routine list of employers does not put tax-identifying
	// data through the browser, the cache and the console on every page load.
	HasEIN    bool    `json:"has_ein"`
	EINMasked *string `json:"ein_masked"`
}

type employerRequest struct {
	Name string `json:"name"`
	// EIN is three-valued and the API depends on the distinction: absent means
	// "leave whatever is stored alone", empty means "remove it", a value means
	// "replace it". A sealed column cannot be read back and compared, so
	// re-sealing on every edit would be the only alternative — and that would
	// silently wipe the EIN of any client that omits the field.
	EIN          *string `json:"ein"`
	Address      *string `json:"address"`
	PayFrequency string  `json:"pay_frequency"`
}

func (s *Server) handleListEmployers(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListEmployers(r.Context(), dbgen.ListEmployersParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID,
	})
	if err != nil {
		s.internalError(w, "list employers", err)
		return
	}

	out := make([]employerResponse, 0, len(rows))
	for _, e := range rows {
		out = append(out, employerResponse{
			ID:           e.ID,
			Name:         e.Name,
			Address:      e.Address,
			PayFrequency: e.PayFrequency,
			PaystubCount: e.PaystubCount,
			HasEIN:       len(e.EinEncrypted) > 0,
			EINMasked:    s.maskEIN(e.EinEncrypted),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateEmployer(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req employerRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	name, ok := requireName(w, req.Name)
	if !ok {
		return
	}
	if !payroll.ValidFrequency(req.PayFrequency) {
		writeError(w, http.StatusBadRequest,
			"pay_frequency must be one of weekly, biweekly, semimonthly, monthly")
		return
	}

	sealed, ok := s.sealEIN(w, req.EIN)
	if !ok {
		return
	}

	row, err := s.Queries.CreateEmployer(r.Context(), dbgen.CreateEmployerParams{
		HouseholdID:  identity.HouseholdID,
		Name:         name,
		EinEncrypted: sealed,
		Address:      nilIfEmptyPtr(req.Address),
		PayFrequency: req.PayFrequency,
	})
	if isUniqueViolation(err) {
		writeError(w, http.StatusConflict, "an employer with that name already exists")
		return
	}
	if err != nil {
		s.internalError(w, "create employer", err)
		return
	}

	writeJSON(w, http.StatusCreated, employerResponse{
		ID: row.ID, Name: row.Name, Address: row.Address,
		PayFrequency: row.PayFrequency,
		HasEIN:       len(row.EinEncrypted) > 0,
		EINMasked:    s.maskEIN(row.EinEncrypted),
	})
}

func (s *Server) handleUpdateEmployer(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	employerID, ok := pathUUID(w, r, "employerID")
	if !ok {
		return
	}

	var req employerRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	name, ok := requireName(w, req.Name)
	if !ok {
		return
	}
	if !payroll.ValidFrequency(req.PayFrequency) {
		writeError(w, http.StatusBadRequest,
			"pay_frequency must be one of weekly, biweekly, semimonthly, monthly")
		return
	}

	existing, err := s.Queries.GetEmployer(r.Context(), dbgen.GetEmployerParams{
		ID: employerID, HouseholdID: identity.HouseholdID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "employer not found")
		return
	}
	if err != nil {
		s.internalError(w, "load employer", err)
		return
	}

	// The three-valued EIN, resolved here so the query stays a plain UPDATE.
	sealed := existing.EinEncrypted
	if req.EIN != nil {
		if sealed, ok = s.sealEIN(w, req.EIN); !ok {
			return
		}
	}

	row, err := s.Queries.UpdateEmployer(r.Context(), dbgen.UpdateEmployerParams{
		ID:           employerID,
		HouseholdID:  identity.HouseholdID,
		Name:         name,
		EinEncrypted: sealed,
		Address:      nilIfEmptyPtr(req.Address),
		PayFrequency: req.PayFrequency,
	})
	if isUniqueViolation(err) {
		writeError(w, http.StatusConflict, "an employer with that name already exists")
		return
	}
	if err != nil {
		s.internalError(w, "update employer", err)
		return
	}

	writeJSON(w, http.StatusOK, employerResponse{
		ID: row.ID, Name: row.Name, Address: row.Address,
		PayFrequency: row.PayFrequency,
		HasEIN:       len(row.EinEncrypted) > 0,
		EINMasked:    s.maskEIN(row.EinEncrypted),
	})
}

func (s *Server) handleDeleteEmployer(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	employerID, ok := pathUUID(w, r, "employerID")
	if !ok {
		return
	}

	// The foreign key cascades, and that is exactly why this check exists. A
	// household tidying up a duplicate employer must not discover afterwards
	// that it deleted a year of payroll history as a side effect — including
	// stubs belonging to the OTHER member, which this caller cannot even see.
	count, err := s.Queries.CountEmployerPaystubs(r.Context(), employerID)
	if err != nil {
		s.internalError(w, "count employer paystubs", err)
		return
	}
	if count > 0 {
		writeError(w, http.StatusConflict,
			"this employer still has "+strconv.FormatInt(count, 10)+
				" paystub(s). Delete them first — removing the employer would take them with it.")
		return
	}

	affected, err := s.Queries.DeleteEmployer(r.Context(), dbgen.DeleteEmployerParams{
		ID: employerID, HouseholdID: identity.HouseholdID,
	})
	if err != nil {
		s.internalError(w, "delete employer", err)
		return
	}
	if affected == 0 {
		writeError(w, http.StatusNotFound, "employer not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sealEIN encrypts an EIN for storage, or returns nil for an absent/empty one.
//
// Digits only: an EIN is nine of them, and the hyphen is presentation. Storing
// what the user typed would mean two employers with the same EIN comparing
// unequal, and the value is sealed so nothing could ever normalise it later.
func (s *Server) sealEIN(w http.ResponseWriter, raw *string) ([]byte, bool) {
	if raw == nil || *raw == "" {
		return nil, true
	}
	digits := digitsOnly(*raw)
	if len(digits) != 9 {
		writeError(w, http.StatusBadRequest, "an EIN is nine digits, as 12-3456789")
		return nil, false
	}
	sealed, err := s.Cipher.Seal([]byte(digits))
	if err != nil {
		s.internalError(w, "seal employer EIN", err)
		return nil, false
	}
	return sealed, true
}

// openEIN decrypts a stored EIN into its hyphenated form.
//
// A decryption failure is reported as "not available" rather than as an error.
// It means the row was written under a different ENCRYPTION_KEY, which is a
// recoverable state — the user retypes nine digits off their W-2 — and failing
// the whole tax summary over one unreadable field would not be.
func (s *Server) openEIN(sealed []byte) *string {
	if len(sealed) == 0 {
		return nil
	}
	plain, err := s.Cipher.Open(sealed)
	if err != nil || len(plain) != 9 {
		return nil
	}
	formatted := string(plain[:2]) + "-" + string(plain[2:])
	return &formatted
}

// maskEIN renders a stored EIN as "**-***6789" for a listing.
func (s *Server) maskEIN(sealed []byte) *string {
	full := s.openEIN(sealed)
	if full == nil {
		return nil
	}
	masked := "**-***" + (*full)[len(*full)-4:]
	return &masked
}

// --------------------------------------------------------------------------
// Paystubs
// --------------------------------------------------------------------------

type paystubLineResponse struct {
	ID       uuid.UUID `json:"id"`
	Category string    `json:"category"`
	// Label is the employer's own wording; CategoryLabel is the taxonomy's. The
	// UI shows the former and groups by the latter.
	Label         string  `json:"label"`
	CategoryLabel string  `json:"category_label"`
	Group         string  `json:"group"`
	Amount        string  `json:"amount"`
	YTDAmount     *string `json:"ytd_amount"`
	PreTax        bool    `json:"pre_tax"`
	IsEmployer    bool    `json:"is_employer"`
	IsTax         bool    `json:"is_tax"`
}

type depositResponse struct {
	TransactionID uuid.UUID `json:"transaction_id"`
	Date          string    `json:"date"`
	Amount        string    `json:"amount"`
}

type paystubResponse struct {
	ID           uuid.UUID `json:"id"`
	EmployerID   uuid.UUID `json:"employer_id"`
	EmployerName string    `json:"employer_name"`
	PayFrequency string    `json:"pay_frequency"`

	PeriodStart string `json:"period_start"`
	PeriodEnd   string `json:"period_end"`
	PayDate     string `json:"pay_date"`

	Gross    string  `json:"gross"`
	Net      string  `json:"net"`
	YTDGross *string `json:"ytd_gross"`
	YTDNet   *string `json:"ytd_net"`

	Source      string  `json:"source"`
	Confirmed   bool    `json:"confirmed"`
	ConfirmedAt *string `json:"confirmed_at"`
	IsShared    bool    `json:"is_shared"`
	// IsOwn tells the UI whether to render controls at all. It is presentation
	// only — every mutation is refused server-side for a stub the caller does
	// not own, whatever the client renders.
	IsOwn      bool       `json:"is_own"`
	DocumentID *uuid.UUID `json:"document_id"`

	Deposit *depositResponse `json:"deposit"`

	Lines []paystubLineResponse `json:"lines"`

	// Derived, all of it finished server-side in exact decimal.
	TaxTotal          string           `json:"tax_total"`
	EffectiveTaxRate  *decimal.Decimal `json:"effective_tax_rate"`
	EmployerTotal     string           `json:"employer_total"`
	TotalCompensation string           `json:"total_compensation"`
	// Balances and Residual are the review screen's headline. A stub that does
	// not balance cannot be confirmed, so the UI has to be able to say by how
	// much and where to look.
	Balances  bool                    `json:"balances"`
	Residual  string                  `json:"residual"`
	Breakdown []breakdownBandResponse `json:"breakdown"`
}

type breakdownBandResponse struct {
	Group  string `json:"group"`
	Label  string `json:"label"`
	Amount string `json:"amount"`
}

// breakdownGroupLabels names the bands of the "where your paycheck went"
// chart. Server-side so the chart's legend and the API agree by construction.
var breakdownGroupLabels = map[string]string{
	"tax":        "Taxes",
	"retirement": "Retirement",
	"health":     "Health & benefits",
	"insurance":  "Insurance",
	"other":      "Other deductions",
}

type paystubLineRequest struct {
	Category   string  `json:"category"`
	Label      string  `json:"label"`
	Amount     string  `json:"amount"`
	YTDAmount  *string `json:"ytd_amount"`
	PreTax     bool    `json:"pre_tax"`
	IsEmployer bool    `json:"is_employer"`
}

type paystubRequest struct {
	EmployerID  uuid.UUID `json:"employer_id"`
	PeriodStart string    `json:"period_start"`
	PeriodEnd   string    `json:"period_end"`
	PayDate     string    `json:"pay_date"`
	Gross       string    `json:"gross"`
	Net         string    `json:"net"`
	YTDGross    *string   `json:"ytd_gross"`
	YTDNet      *string   `json:"ytd_net"`
	// Source is 'manual' or 'pdf' and is only read on create. It records how
	// the figures got here, which is what makes "imported stubs were reviewed"
	// an answerable question later.
	Source string `json:"source"`
	// Confirm asks for the stub to count towards reported figures. Honoured
	// only if the stub balances.
	Confirm    bool                 `json:"confirm"`
	IsShared   bool                 `json:"is_shared"`
	DocumentID *uuid.UUID           `json:"document_id"`
	Lines      []paystubLineRequest `json:"lines"`
}

func (s *Server) handleListPaystubs(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	year, byYear, ok := optionalYearParam(w, r)
	if !ok {
		return
	}

	rows, err := s.Queries.ListPaystubs(r.Context(), dbgen.ListPaystubsParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		ByYear:      byYear,
		TaxYear:     year,
	})
	if err != nil {
		s.internalError(w, "list paystubs", err)
		return
	}

	ids := make([]uuid.UUID, 0, len(rows))
	for _, p := range rows {
		ids = append(ids, p.ID)
	}
	linesByStub, err := s.paystubLines(r, ids)
	if err != nil {
		s.internalError(w, "list paystub lines", err)
		return
	}

	out := make([]paystubResponse, 0, len(rows))
	for _, p := range rows {
		out = append(out, buildPaystubResponse(paystubView{
			ID: p.ID, UserID: p.UserID, EmployerID: p.EmployerID,
			EmployerName: p.EmployerName, PayFrequency: p.PayFrequency,
			PeriodStart: p.PeriodStart, PeriodEnd: p.PeriodEnd, PayDate: p.PayDate,
			Gross: p.Gross, Net: p.Net, YTDGross: p.YtdGross, YTDNet: p.YtdNet,
			Source: p.Source, ConfirmedAt: p.ConfirmedAt, IsShared: p.IsShared,
			TransactionID: p.TransactionID, DocumentID: p.DocumentID,
			DepositDate: p.DepositDate, DepositAmount: p.DepositAmount,
		}, linesByStub[p.ID], identity.UserID))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetPaystub(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	paystubID, ok := pathUUID(w, r, "paystubID")
	if !ok {
		return
	}

	p, err := s.Queries.GetPaystub(r.Context(), dbgen.GetPaystubParams{
		ID: paystubID, HouseholdID: identity.HouseholdID, UserID: identity.UserID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "paystub not found")
		return
	}
	if err != nil {
		s.internalError(w, "get paystub", err)
		return
	}

	linesByStub, err := s.paystubLines(r, []uuid.UUID{p.ID})
	if err != nil {
		s.internalError(w, "list paystub lines", err)
		return
	}

	writeJSON(w, http.StatusOK, buildPaystubResponse(paystubView{
		ID: p.ID, UserID: p.UserID, EmployerID: p.EmployerID,
		EmployerName: p.EmployerName, PayFrequency: p.PayFrequency,
		PeriodStart: p.PeriodStart, PeriodEnd: p.PeriodEnd, PayDate: p.PayDate,
		Gross: p.Gross, Net: p.Net, YTDGross: p.YtdGross, YTDNet: p.YtdNet,
		Source: p.Source, ConfirmedAt: p.ConfirmedAt, IsShared: p.IsShared,
		TransactionID: p.TransactionID, DocumentID: p.DocumentID,
		DepositDate: p.DepositDate, DepositAmount: p.DepositAmount,
	}, linesByStub[p.ID], identity.UserID))
}

func (s *Server) handleCreatePaystub(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req paystubRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	parsed, ok := s.validatePaystubRequest(w, r, req, true)
	if !ok {
		return
	}

	// One transaction for the stub and its lines. A half-written stub — header
	// stored, deductions lost — would balance to exactly its own net pay and
	// look perfectly fine on screen.
	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		s.internalError(w, "begin paystub write", err)
		return
	}
	defer tx.Rollback(r.Context())
	qtx := s.Queries.WithTx(tx)

	stub, err := qtx.CreatePaystub(r.Context(), dbgen.CreatePaystubParams{
		UserID:      identity.UserID,
		EmployerID:  req.EmployerID,
		PeriodStart: parsed.periodStart,
		PeriodEnd:   parsed.periodEnd,
		PayDate:     parsed.payDate,
		Gross:       parsed.gross,
		Net:         parsed.net,
		YtdGross:    parsed.ytdGross,
		YtdNet:      parsed.ytdNet,
		Source:      parsed.source,
		ConfirmedAt: parsed.confirmedAt,
		IsShared:    req.IsShared,
		DocumentID:  req.DocumentID,
	})
	if isUniqueViolation(err) {
		writeError(w, http.StatusConflict,
			"a paystub for that employer and pay date already exists")
		return
	}
	if err != nil {
		s.internalError(w, "create paystub", err)
		return
	}

	if err := writePaystubLines(r, qtx, stub.ID, parsed.lines); err != nil {
		s.internalError(w, "write paystub lines", err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.internalError(w, "commit paystub", err)
		return
	}

	s.respondWithPaystub(w, r, stub.ID)
}

func (s *Server) handleUpdatePaystub(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	paystubID, ok := pathUUID(w, r, "paystubID")
	if !ok {
		return
	}
	var req paystubRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Ownership is checked before anything is validated, so a caller who cannot
	// edit the row learns that rather than which fields it would have rejected.
	if _, ok := s.ownedPaystub(w, r, paystubID); !ok {
		return
	}
	parsed, ok := s.validatePaystubRequest(w, r, req, false)
	if !ok {
		return
	}

	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		s.internalError(w, "begin paystub update", err)
		return
	}
	defer tx.Rollback(r.Context())
	qtx := s.Queries.WithTx(tx)

	if _, err := qtx.UpdatePaystub(r.Context(), dbgen.UpdatePaystubParams{
		ID:          paystubID,
		UserID:      identity.UserID,
		EmployerID:  req.EmployerID,
		PeriodStart: parsed.periodStart,
		PeriodEnd:   parsed.periodEnd,
		PayDate:     parsed.payDate,
		Gross:       parsed.gross,
		Net:         parsed.net,
		YtdGross:    parsed.ytdGross,
		YtdNet:      parsed.ytdNet,
	}); err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict,
				"a paystub for that employer and pay date already exists")
			return
		}
		s.internalError(w, "update paystub", err)
		return
	}

	// Lines are replaced wholesale — see the query comment. Diffing them would
	// risk leaving a stale line behind, and a stale line is a stub that no
	// longer balances.
	if err := qtx.DeletePaystubLines(r.Context(), paystubID); err != nil {
		s.internalError(w, "clear paystub lines", err)
		return
	}
	if err := writePaystubLines(r, qtx, paystubID, parsed.lines); err != nil {
		s.internalError(w, "write paystub lines", err)
		return
	}

	// An edit re-opens the review. Editing the figures of a confirmed stub and
	// leaving it confirmed would mean a number that was reviewed and a number
	// that is reported drifting apart silently; the client re-confirms, which
	// is one click and an honest record of when the current figures were
	// approved.
	if _, err := qtx.SetPaystubConfirmed(r.Context(), dbgen.SetPaystubConfirmedParams{
		ID: paystubID, UserID: identity.UserID, Confirmed: parsed.confirmedAt != nil,
	}); err != nil {
		s.internalError(w, "reset paystub confirmation", err)
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		s.internalError(w, "commit paystub update", err)
		return
	}
	s.respondWithPaystub(w, r, paystubID)
}

func (s *Server) handleDeletePaystub(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	paystubID, ok := pathUUID(w, r, "paystubID")
	if !ok {
		return
	}
	affected, err := s.Queries.DeletePaystub(r.Context(), dbgen.DeletePaystubParams{
		ID: paystubID, UserID: identity.UserID,
	})
	if err != nil {
		s.internalError(w, "delete paystub", err)
		return
	}
	if affected == 0 {
		writeError(w, http.StatusNotFound, "paystub not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type confirmRequest struct {
	Confirmed bool `json:"confirmed"`
}

func (s *Server) handleConfirmPaystub(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	paystubID, ok := pathUUID(w, r, "paystubID")
	if !ok {
		return
	}
	var req confirmRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	stub, ok := s.ownedPaystub(w, r, paystubID)
	if !ok {
		return
	}

	if req.Confirmed {
		lines, err := s.Queries.ListPaystubLines(r.Context(), paystubID)
		if err != nil {
			s.internalError(w, "list paystub lines", err)
			return
		}
		domain := payroll.Stub{Gross: stub.Gross, Net: stub.Net, Lines: toDomainLines(lines)}
		if !domain.Balances() {
			// 422 rather than 400: the request is well-formed and the stored
			// stub is what is wrong with it.
			writeError(w, http.StatusUnprocessableEntity,
				"this paystub does not balance — gross minus the deductions is "+
					domain.Residual().Abs().StringFixed(2)+
					" away from net. Fix the figures before confirming; an unbalanced "+
					"stub would put that gap into every total derived from it.")
			return
		}
	}

	if _, err := s.Queries.SetPaystubConfirmed(r.Context(), dbgen.SetPaystubConfirmedParams{
		ID: paystubID, UserID: identity.UserID, Confirmed: req.Confirmed,
	}); err != nil {
		s.internalError(w, "set paystub confirmation", err)
		return
	}
	s.respondWithPaystub(w, r, paystubID)
}

type paystubSharingRequest struct {
	IsShared bool `json:"is_shared"`
}

func (s *Server) handleSetPaystubSharing(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	paystubID, ok := pathUUID(w, r, "paystubID")
	if !ok {
		return
	}
	var req paystubSharingRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, ok := s.ownedPaystub(w, r, paystubID); !ok {
		return
	}
	if _, err := s.Queries.SetPaystubSharing(r.Context(), dbgen.SetPaystubSharingParams{
		ID: paystubID, UserID: identity.UserID, IsShared: req.IsShared,
	}); err != nil {
		s.internalError(w, "set paystub sharing", err)
		return
	}
	s.respondWithPaystub(w, r, paystubID)
}

// --------------------------------------------------------------------------
// Deposit reconciliation
// --------------------------------------------------------------------------

type depositMatchResponse struct {
	TransactionID uuid.UUID `json:"transaction_id"`
	Date          string    `json:"date"`
	// Amount is the deposit as money IN — the sign is flipped from Plaid's
	// convention here so the client never has to know about it.
	Amount      string `json:"amount"`
	Label       string `json:"label"`
	AccountName string `json:"account_name"`
	// Delta is how far the deposit is from the stub's net pay. Zero is an exact
	// match; anything else is shown, because a split direct deposit is a
	// legitimate partial match and hiding the gap would make it look like an
	// exact one.
	Delta string `json:"delta"`
	Exact bool   `json:"exact"`
}

func (s *Server) handleMatchPaystubDeposit(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	paystubID, ok := pathUUID(w, r, "paystubID")
	if !ok {
		return
	}
	// The shared-visibility read: proposing a match for a stub you can see is
	// harmless, and refusing it would make the reconciliation state unreadable
	// for the household member the stub was shared WITH.
	stub, err := s.Queries.GetPaystub(r.Context(), dbgen.GetPaystubParams{
		ID: paystubID, HouseholdID: identity.HouseholdID, UserID: identity.UserID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "paystub not found")
		return
	}
	if err != nil {
		s.internalError(w, "get paystub", err)
		return
	}

	rows, err := s.Queries.MatchDepositsForPaystub(r.Context(), dbgen.MatchDepositsForPaystubParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Net:         stub.Net,
		PayDate:     stub.PayDate,
		FromDate:    stub.PayDate.AddDate(0, 0, -depositMatchWindowDays),
		ToDate:      stub.PayDate.AddDate(0, 0, depositMatchWindowDays),
		PaystubID:   paystubID,
		MaxResults:  maxDepositMatches,
	})
	if err != nil {
		s.internalError(w, "match paystub deposit", err)
		return
	}

	out := make([]depositMatchResponse, 0, len(rows))
	for _, m := range rows {
		out = append(out, depositMatchResponse{
			TransactionID: m.ID,
			Date:          m.Date.Format(time.DateOnly),
			Amount:        m.Amount.Neg().StringFixed(2),
			Label:         m.Label,
			AccountName:   m.AccountName,
			Delta:         m.Delta.StringFixed(2),
			Exact:         m.Delta.IsZero(),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type linkDepositRequest struct {
	// Null clears the link. A wrong match is worse than none, so unlinking has
	// to be as easy as linking.
	TransactionID *uuid.UUID `json:"transaction_id"`
}

func (s *Server) handleLinkPaystubDeposit(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	paystubID, ok := pathUUID(w, r, "paystubID")
	if !ok {
		return
	}
	var req linkDepositRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, ok := s.ownedPaystub(w, r, paystubID); !ok {
		return
	}

	if _, err := s.Queries.SetPaystubTransaction(r.Context(), dbgen.SetPaystubTransactionParams{
		ID: paystubID, UserID: identity.UserID, TransactionID: req.TransactionID,
	}); err != nil {
		s.internalError(w, "link paystub deposit", err)
		return
	}
	s.respondWithPaystub(w, r, paystubID)
}

// --------------------------------------------------------------------------
// Shared plumbing
// --------------------------------------------------------------------------

// paystubView is the union of the columns ListPaystubs and GetPaystub return,
// so one builder serves both rather than two that can drift.
type paystubView struct {
	ID            uuid.UUID
	UserID        uuid.UUID
	EmployerID    uuid.UUID
	EmployerName  string
	PayFrequency  string
	PeriodStart   time.Time
	PeriodEnd     time.Time
	PayDate       time.Time
	Gross         decimal.Decimal
	Net           decimal.Decimal
	YTDGross      decimal.NullDecimal
	YTDNet        decimal.NullDecimal
	Source        string
	ConfirmedAt   *time.Time
	IsShared      bool
	TransactionID *uuid.UUID
	DocumentID    *uuid.UUID
	DepositDate   *time.Time
	DepositAmount decimal.NullDecimal
}

func buildPaystubResponse(p paystubView, lines []dbgen.PaystubLine, callerID uuid.UUID) paystubResponse {
	domain := payroll.Stub{Gross: p.Gross, Net: p.Net, Lines: toDomainLines(lines)}

	resp := paystubResponse{
		ID: p.ID, EmployerID: p.EmployerID, EmployerName: p.EmployerName,
		PayFrequency: p.PayFrequency,
		PeriodStart:  p.PeriodStart.Format(time.DateOnly),
		PeriodEnd:    p.PeriodEnd.Format(time.DateOnly),
		PayDate:      p.PayDate.Format(time.DateOnly),
		Gross:        p.Gross.StringFixed(2),
		Net:          p.Net.StringFixed(2),
		YTDGross:     moneyOrNil(p.YTDGross),
		YTDNet:       moneyOrNil(p.YTDNet),
		Source:       p.Source,
		Confirmed:    p.ConfirmedAt != nil,
		IsShared:     p.IsShared,
		IsOwn:        p.UserID == callerID,
		DocumentID:   p.DocumentID,

		TaxTotal:          domain.TaxTotal().StringFixed(2),
		EmployerTotal:     domain.EmployerTotal().StringFixed(2),
		TotalCompensation: domain.TotalCompensation().StringFixed(2),
		Balances:          domain.Balances(),
		Residual:          domain.Residual().StringFixed(2),
		Lines:             make([]paystubLineResponse, 0, len(lines)),
		Breakdown:         make([]breakdownBandResponse, 0, len(payroll.BreakdownGroups)),
	}
	if p.ConfirmedAt != nil {
		at := p.ConfirmedAt.UTC().Format(time.RFC3339)
		resp.ConfirmedAt = &at
	}
	if rate, ok := domain.EffectiveTaxRate(); ok {
		rounded := rate.Round(4)
		resp.EffectiveTaxRate = &rounded
	}
	if p.TransactionID != nil && p.DepositDate != nil && p.DepositAmount.Valid {
		resp.Deposit = &depositResponse{
			TransactionID: *p.TransactionID,
			Date:          p.DepositDate.Format(time.DateOnly),
			Amount:        p.DepositAmount.Decimal.Neg().StringFixed(2),
		}
	}

	for _, l := range lines {
		spec := payroll.SpecFor(payroll.Category(l.Category))
		resp.Lines = append(resp.Lines, paystubLineResponse{
			ID: l.ID, Category: l.Category, Label: l.Label,
			CategoryLabel: spec.Label, Group: spec.Group,
			Amount:     l.Amount.StringFixed(2),
			YTDAmount:  moneyOrNil(l.YtdAmount),
			PreTax:     l.PreTax,
			IsEmployer: l.IsEmployer,
			IsTax:      spec.IsTax,
		})
	}

	bands, _ := domain.Breakdown()
	for _, b := range bands {
		resp.Breakdown = append(resp.Breakdown, breakdownBandResponse{
			Group: b.Group, Label: breakdownGroupLabels[b.Group],
			Amount: b.Amount.StringFixed(2),
		})
	}
	return resp
}

// toDomainLines converts stored rows into the shape the arithmetic works on.
func toDomainLines(rows []dbgen.PaystubLine) []payroll.Line {
	out := make([]payroll.Line, 0, len(rows))
	for _, l := range rows {
		out = append(out, payroll.Line{
			Category:   payroll.Category(l.Category),
			Label:      l.Label,
			Amount:     l.Amount,
			YTDAmount:  l.YtdAmount,
			PreTax:     l.PreTax,
			IsEmployer: l.IsEmployer,
		})
	}
	return out
}

// parsedPaystub is a validated request, ready for the database.
type parsedPaystub struct {
	periodStart, periodEnd, payDate time.Time
	gross, net                      decimal.Decimal
	ytdGross, ytdNet                decimal.NullDecimal
	source                          string
	confirmedAt                     *time.Time
	lines                           []dbgen.CreatePaystubLineParams
}

// validatePaystubRequest checks everything before a row is written, so a bad
// payload is a 400 naming the field rather than a 500 naming a constraint.
func (s *Server) validatePaystubRequest(
	w http.ResponseWriter, r *http.Request, req paystubRequest, isCreate bool,
) (parsedPaystub, bool) {
	var out parsedPaystub
	identity := auth.MustFromContext(r.Context())

	if req.EmployerID == uuid.Nil {
		writeError(w, http.StatusBadRequest, "employer_id is required")
		return out, false
	}
	// The employer must be this household's. Without this check an employer id
	// from anywhere would be accepted and the stub would join to a name the
	// caller has no business seeing.
	if _, err := s.Queries.GetEmployer(r.Context(), dbgen.GetEmployerParams{
		ID: req.EmployerID, HouseholdID: identity.HouseholdID,
	}); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusBadRequest, "employer not found")
		return out, false
	} else if err != nil {
		s.internalError(w, "load employer", err)
		return out, false
	}

	var ok bool
	if out.payDate, ok = requireDate(w, req.PayDate, "pay_date"); !ok {
		return out, false
	}
	if out.periodStart, ok = requireDate(w, req.PeriodStart, "period_start"); !ok {
		return out, false
	}
	if out.periodEnd, ok = requireDate(w, req.PeriodEnd, "period_end"); !ok {
		return out, false
	}
	if out.periodEnd.Before(out.periodStart) {
		writeError(w, http.StatusBadRequest, "period_end cannot be before period_start")
		return out, false
	}

	if out.gross, ok = requireMoney(w, req.Gross, "gross"); !ok {
		return out, false
	}
	if out.net, ok = requireMoney(w, req.Net, "net"); !ok {
		return out, false
	}
	if out.gross.IsNegative() || out.net.IsNegative() {
		writeError(w, http.StatusBadRequest, "gross and net cannot be negative")
		return out, false
	}
	if out.ytdGross, ok = optionalPayrollMoney(w, req.YTDGross, "ytd_gross"); !ok {
		return out, false
	}
	if out.ytdNet, ok = optionalPayrollMoney(w, req.YTDNet, "ytd_net"); !ok {
		return out, false
	}

	if isCreate {
		switch req.Source {
		case "manual", "pdf":
			out.source = req.Source
		case "":
			out.source = "manual"
		default:
			writeError(w, http.StatusBadRequest, "source must be 'manual' or 'pdf'")
			return out, false
		}
	}

	domainLines := make([]payroll.Line, 0, len(req.Lines))
	for i, l := range req.Lines {
		field := "lines[" + strconv.Itoa(i) + "]"
		if !payroll.Valid(l.Category) {
			writeError(w, http.StatusBadRequest, field+".category is not a known deduction category")
			return out, false
		}
		amount, ok := requireMoney(w, l.Amount, field+".amount")
		if !ok {
			return out, false
		}
		if amount.IsNegative() {
			writeError(w, http.StatusBadRequest,
				field+".amount must be positive — a deduction's direction comes from its category, not its sign")
			return out, false
		}
		ytd, ok := optionalPayrollMoney(w, l.YTDAmount, field+".ytd_amount")
		if !ok {
			return out, false
		}

		spec := payroll.SpecFor(payroll.Category(l.Category))
		// The taxonomy overrides the client on the two things it, and not the
		// client, is authoritative about: an employer match is employer money
		// whatever the request says, and a category that cannot be pre-tax is
		// not made so by a checkbox.
		isEmployer := l.IsEmployer || spec.EmployerOnly
		preTax := l.PreTax && spec.Basis != payroll.ReducesNothing

		out.lines = append(out.lines, dbgen.CreatePaystubLineParams{
			Category:   l.Category,
			Label:      lineLabel(l.Label, spec.Label),
			Amount:     amount,
			YtdAmount:  ytd,
			PreTax:     preTax,
			IsEmployer: isEmployer,
		})
		domainLines = append(domainLines, payroll.Line{
			Category: payroll.Category(l.Category), Amount: amount,
			PreTax: preTax, IsEmployer: isEmployer,
		})
	}

	if req.Confirm {
		domain := payroll.Stub{Gross: out.gross, Net: out.net, Lines: domainLines}
		if !domain.Balances() {
			writeError(w, http.StatusUnprocessableEntity,
				"this paystub does not balance — gross minus the deductions is "+
					domain.Residual().Abs().StringFixed(2)+
					" away from net. Save it as a draft and add the missing line, or "+
					"correct the figures; a stub that does not reconcile would put that "+
					"gap into every total derived from it.")
			return out, false
		}
		now := time.Now().UTC()
		out.confirmedAt = &now
	}
	return out, true
}

// lineLabel keeps the employer's own wording where there is one, redacted of
// anything that looks like a personal identifier. Redaction is applied on the
// way IN, not on the way out: an SSN that never reaches a row never reaches a
// backup or an export either.
func lineLabel(given, fallback string) string {
	label := payroll.RedactIdentifiers(clip(given, 80))
	if label == "" {
		return fallback
	}
	return label
}

func writePaystubLines(r *http.Request, q *dbgen.Queries, paystubID uuid.UUID, lines []dbgen.CreatePaystubLineParams) error {
	for _, l := range lines {
		l.PaystubID = paystubID
		if _, err := q.CreatePaystubLine(r.Context(), l); err != nil {
			return err
		}
	}
	return nil
}

// ownedPaystub resolves a stub the caller may MUTATE. Seeing a shared stub is
// not permission to change it.
func (s *Server) ownedPaystub(w http.ResponseWriter, r *http.Request, id uuid.UUID) (dbgen.Paystub, bool) {
	identity := auth.MustFromContext(r.Context())

	stub, err := s.Queries.GetOwnedPaystub(r.Context(), dbgen.GetOwnedPaystubParams{
		ID: id, UserID: identity.UserID, HouseholdID: identity.HouseholdID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// 404 rather than 403 for a stub that exists but belongs to somebody
		// else: the distinction would confirm that another member has a stub,
		// which is the thing the default-private rule is protecting.
		writeError(w, http.StatusNotFound, "paystub not found")
		return dbgen.Paystub{}, false
	}
	if err != nil {
		s.internalError(w, "load paystub", err)
		return dbgen.Paystub{}, false
	}
	return stub, true
}

// paystubLines fetches the lines for a set of stubs in one round trip.
func (s *Server) paystubLines(r *http.Request, ids []uuid.UUID) (map[uuid.UUID][]dbgen.PaystubLine, error) {
	out := map[uuid.UUID][]dbgen.PaystubLine{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.Queries.ListPaystubLinesForStubs(r.Context(), ids)
	if err != nil {
		return nil, err
	}
	for _, l := range rows {
		out[l.PaystubID] = append(out[l.PaystubID], l)
	}
	return out, nil
}

// respondWithPaystub re-reads a stub through the ordinary read path and returns
// it, so a mutation's response is the same shape — and carries the same derived
// figures — as a fetch.
func (s *Server) respondWithPaystub(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	identity := auth.MustFromContext(r.Context())

	p, err := s.Queries.GetPaystub(r.Context(), dbgen.GetPaystubParams{
		ID: id, HouseholdID: identity.HouseholdID, UserID: identity.UserID,
	})
	if err != nil {
		s.internalError(w, "reload paystub", err)
		return
	}
	linesByStub, err := s.paystubLines(r, []uuid.UUID{id})
	if err != nil {
		s.internalError(w, "list paystub lines", err)
		return
	}
	writeJSON(w, http.StatusOK, buildPaystubResponse(paystubView{
		ID: p.ID, UserID: p.UserID, EmployerID: p.EmployerID,
		EmployerName: p.EmployerName, PayFrequency: p.PayFrequency,
		PeriodStart: p.PeriodStart, PeriodEnd: p.PeriodEnd, PayDate: p.PayDate,
		Gross: p.Gross, Net: p.Net, YTDGross: p.YtdGross, YTDNet: p.YtdNet,
		Source: p.Source, ConfirmedAt: p.ConfirmedAt, IsShared: p.IsShared,
		TransactionID: p.TransactionID, DocumentID: p.DocumentID,
		DepositDate: p.DepositDate, DepositAmount: p.DepositAmount,
	}, linesByStub[id], identity.UserID))
}

// --------------------------------------------------------------------------
// Small request helpers
// --------------------------------------------------------------------------

func pathUUID(w http.ResponseWriter, r *http.Request, param string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, param))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid "+param)
		return uuid.Nil, false
	}
	return id, true
}

func requireName(w http.ResponseWriter, raw string) (string, bool) {
	name := clip(raw, 120)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return "", false
	}
	return name, true
}

func requireDate(w http.ResponseWriter, raw, field string) (time.Time, bool) {
	parsed, err := time.Parse(time.DateOnly, raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, field+" must be a date as YYYY-MM-DD")
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func requireMoney(w http.ResponseWriter, raw, field string) (decimal.Decimal, bool) {
	d, err := decimal.NewFromString(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, field+" must be a decimal amount")
		return decimal.Zero, false
	}
	return d, true
}

func optionalPayrollMoney(w http.ResponseWriter, raw *string, field string) (decimal.NullDecimal, bool) {
	if raw == nil || *raw == "" {
		return decimal.NullDecimal{}, true
	}
	d, ok := requireMoney(w, *raw, field)
	if !ok {
		return decimal.NullDecimal{}, false
	}
	return decimal.NullDecimal{Decimal: d, Valid: true}, true
}

func moneyOrNil(d decimal.NullDecimal) *string {
	if !d.Valid {
		return nil
	}
	s := d.Decimal.StringFixed(2)
	return &s
}

func nilIfEmptyPtr(s *string) *string {
	if s == nil {
		return nil
	}
	return nilIfEmpty(*s)
}

func digitsOnly(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			out = append(out, r)
		}
	}
	return string(out)
}

// optionalYearParam reads ?year=YYYY. Absent means "every year", which is what
// the paystub list opens on.
func optionalYearParam(w http.ResponseWriter, r *http.Request) (int32, bool, bool) {
	raw := r.URL.Query().Get("year")
	if raw == "" {
		return 0, false, true
	}
	year, err := strconv.Atoi(raw)
	if err != nil || year < 1900 || year > 2200 {
		writeError(w, http.StatusBadRequest, "year must be a four-digit year")
		return 0, false, false
	}
	return int32(year), true, true
}
