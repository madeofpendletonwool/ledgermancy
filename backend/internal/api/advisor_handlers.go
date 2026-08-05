package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/advisor"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/ai"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
)

// The advisor surface: one GET that computes the household's ranked options on
// demand.
//
// There is no write here and there never will be. The advisor EXECUTES NOTHING —
// no transfers, no payments, ever — so every endpoint it needs is a read. Its
// three settings (threshold, emergency-fund months, suppressed options) are
// household preferences and go through PUT /api/preferences like every other
// household knob; a second settings endpoint would be a second place for them to
// drift.

// advisorOptionResponse is one option on the wire. Money is a STRING, finished
// to the cent, matching every other money-bearing response in this API — a JSON
// number would hand the browser a float to re-round.
type advisorOptionResponse struct {
	Key       string `json:"key"`
	Kind      string `json:"kind"`
	SubjectID string `json:"subject_id,omitempty"`
	Tier      int    `json:"tier"`
	Label     string `json:"label"`
	Detail    string `json:"detail"`
	Amount    string `json:"amount"`
	Value     string `json:"value"`
	ValueKind string `json:"value_kind"`
	Unranked  bool   `json:"unranked"`
	Tradeoff  bool   `json:"tradeoff"`
	Unbounded bool   `json:"unbounded"`
	Note      string `json:"note,omitempty"`
	// Basis is the "how this was calculated" panel. Always present, even when
	// the UI collapses it: it is what separates this feature from a horoscope.
	Basis []advisorBasisResponse `json:"basis"`
}

type advisorBasisResponse struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type advisorResponse struct {
	Slack              string `json:"slack"`
	SlackBasis         string `json:"slack_basis"`
	ObligationCoverage int    `json:"obligation_coverage"`
	IncomeMonths       int    `json:"income_months"`
	Threshold          string `json:"threshold"`
	Significant        bool   `json:"significant"`
	Hurdle             string `json:"hurdle"`
	HurdleBasis        string `json:"hurdle_basis"`

	SlackParts advisorSlackPartsResponse `json:"slack_parts"`

	Options    []advisorOptionResponse `json:"options"`
	Suppressed []string                `json:"suppressed"`

	// Narrative is the model's phrasing of the list above. EMPTY IS A NORMAL
	// ANSWER, not an error: with no API key configured, or on any model
	// failure, the client renders the options as a plain list. The feature works
	// with no key, like everything else here.
	Narrative string `json:"narrative"`
}

type advisorSlackPartsResponse struct {
	ExpectedIncome        string `json:"expected_income"`
	FixedCosts            string `json:"fixed_costs"`
	FixedCostsAfterBills  string `json:"fixed_costs_after_bills"`
	BudgetedDiscretionary string `json:"budgeted_discretionary"`
	GoalContributions     string `json:"goal_contributions"`
	UpcomingObligations   string `json:"upcoming_obligations"`
}

// handleAdvisor computes and returns the household's ranked options.
func (s *Server) handleAdvisor(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	identity := auth.MustFromContext(ctx)
	now := time.Now().UTC()

	adv, err := advisor.Build(ctx, s.Queries, identity.HouseholdID, now)
	if err != nil {
		s.internalError(w, "build advisor options", err)
		return
	}

	resp := advisorResponse{
		Slack:              advisorMoney(adv.Slack),
		SlackBasis:         adv.SlackBasis,
		ObligationCoverage: adv.ObligationCoverage,
		IncomeMonths:       adv.IncomeMonths,
		Threshold:          advisorMoney(adv.Threshold),
		Significant:        adv.Significant,
		Hurdle:             adv.Hurdle.Round(2).String(),
		HurdleBasis:        adv.HurdleBasis,
		SlackParts: advisorSlackPartsResponse{
			ExpectedIncome:        advisorMoney(adv.SlackParts.ExpectedIncome),
			FixedCosts:            advisorMoney(adv.SlackParts.FixedCosts),
			FixedCostsAfterBills:  advisorMoney(adv.SlackParts.FixedCostsAfterBills),
			BudgetedDiscretionary: advisorMoney(adv.SlackParts.BudgetedDiscretionary),
			GoalContributions:     advisorMoney(adv.SlackParts.GoalContributions),
			UpcomingObligations:   advisorMoney(adv.SlackParts.UpcomingObligations),
		},
		Options:    make([]advisorOptionResponse, 0, len(adv.Options)),
		Suppressed: adv.Suppressed,
	}

	for _, o := range adv.Options {
		basis := make([]advisorBasisResponse, 0, len(o.Basis))
		for _, b := range o.Basis {
			basis = append(basis, advisorBasisResponse{Label: b.Label, Value: b.Value})
		}
		resp.Options = append(resp.Options, advisorOptionResponse{
			Key:       o.Key,
			Kind:      o.Kind,
			SubjectID: o.SubjectID,
			Tier:      o.Tier,
			Label:     o.Label,
			Detail:    o.Detail,
			Amount:    advisorMoney(o.Amount),
			// Value carries months for the goal option, so it is rendered as a
			// bare figure and value_kind tells the client which it is. Rendering
			// "3 months earlier" as "$3.00" is the kind of mistake that makes a
			// panel untrustworthy.
			Value:     o.Value.Round(2).String(),
			ValueKind: o.ValueKind,
			Unranked:  o.Unranked,
			Tradeoff:  o.Tradeoff,
			Unbounded: o.Unbounded,
			Note:      o.Note,
			Basis:     basis,
		})
	}

	// Narration last, and never fatal. The deterministic list is the product;
	// the prose is decoration on top of it.
	if len(resp.Options) > 0 {
		resp.Narrative = s.narrateAdvice(ctx, adv, resp.Options)
	}

	writeJSON(w, http.StatusOK, resp)
}

// narrateAdvice asks the model to read the finished list aloud, returning ""
// on ErrDisabled or any failure.
//
// The options handed over are the ALREADY-SERIALISED ones, in the order the
// ranker put them in, so the prose cannot describe a different list from the one
// rendered beside it.
func (s *Server) narrateAdvice(ctx context.Context, adv advisor.Advice, opts []advisorOptionResponse) string {
	in := ai.AdvisorInput{
		Slack:       advisorMoney(adv.Slack),
		SlackBasis:  slackBasisWords(adv.SlackBasis),
		Hurdle:      adv.Hurdle.Round(2).String() + "%",
		HurdleBasis: adv.HurdleBasis,
		Options:     make([]ai.AdvisorOption, 0, len(opts)),
	}
	for i, o := range opts {
		in.Options = append(in.Options, ai.AdvisorOption{
			Rank:      i + 1,
			Tier:      o.Tier,
			Label:     o.Label,
			Amount:    o.Amount,
			Value:     o.Value,
			ValueKind: o.ValueKind,
			Detail:    o.Detail,
			Note:      o.Note,
			Tradeoff:  o.Tradeoff,
			Unranked:  o.Unranked,
		})
	}

	text, err := s.AI.AdvisorNarration(ctx, in)
	if err != nil {
		// ErrDisabled is the ordinary no-key path and is not worth a log line at
		// warn; anything else is a real model failure the operator may want.
		if !errors.Is(err, ai.ErrDisabled) {
			slog.Debug("advisor narration fell back to the plain list", "error", err)
		}
		return ""
	}
	return text
}

// slackBasisWords turns the machine label into something the model may repeat.
func slackBasisWords(basis string) string {
	if basis == advisor.SlackBasisAfterBills {
		return "after this month's known bills"
	}
	return "a typical month, from the trailing median"
}

// advisorMoney renders a decimal for this API's JSON: finished to the cent, as
// a string, so the browser is never handed a float to re-round. Deliberately
// NOT moneyfmt.USD — that one decorates prose with "$" and thousands
// separators, and a response field is parsed, not read.
func advisorMoney(d decimal.Decimal) string { return d.Round(2).StringFixed(2) }
