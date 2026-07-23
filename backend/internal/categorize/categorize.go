// Package categorize resolves which category a transaction belongs to.
//
// The resolution order is deliberately cheapest-and-most-certain first:
//
//  1. a manual choice the user already made      (never overridden)
//  2. a household rule                           (explicit user intent)
//  3. the merchant cache                         (a decision already made once)
//  4. Plaid's own Personal Finance Category      (free, and usually right)
//  5. an LLM                                     (phase 6; result gets cached)
//
// Steps 1–4 are deterministic, cost nothing, and handle the overwhelming
// majority of transactions — which is why the AI layer stays optional.
package categorize

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// Source records how a category was decided, for display and for deciding
// what may be overwritten later.
type Source string

const (
	SourceManual Source = "manual"
	SourceRule   Source = "rule"
	SourceCache  Source = "cache"
	SourcePlaid  Source = "plaid"
	SourceLLM    Source = "llm"
)

// Input is what the resolver needs to categorise one transaction.
type Input struct {
	MerchantKey  string
	MerchantName string
	Name         string
	PFCPrimary   string
	PFCDetailed  string
}

// Result is a resolved category.
type Result struct {
	CategoryID uuid.UUID
	Source     Source
}

// Resolver categorises transactions for a single household.
//
// Rules are loaded once per resolver rather than per transaction: a sync can
// process thousands of rows, and re-reading a handful of rules each time would
// dominate the work.
type Resolver struct {
	queries     *dbgen.Queries
	householdID uuid.UUID
	rules       []compiledRule
	fallbackID  uuid.UUID
}

type compiledRule struct {
	categoryID uuid.UUID
	matchType  string
	pattern    string
	lowered    string
	regexp     *regexp.Regexp
}

// NewResolver loads the household's rules and the uncategorised fallback.
func NewResolver(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID) (*Resolver, error) {
	rules, err := q.ListCategoryRules(ctx, householdID)
	if err != nil {
		return nil, fmt.Errorf("load category rules: %w", err)
	}

	compiled := make([]compiledRule, 0, len(rules))
	for _, r := range rules {
		c := compiledRule{
			categoryID: r.CategoryID,
			matchType:  r.MatchType,
			pattern:    r.Pattern,
			lowered:    strings.ToLower(r.Pattern),
		}
		if r.MatchType == "regex" {
			// A user-authored pattern can be invalid. Skip it rather than
			// failing the whole sync over one bad rule.
			re, err := regexp.Compile("(?i)" + r.Pattern)
			if err != nil {
				continue
			}
			c.regexp = re
		}
		compiled = append(compiled, c)
	}

	fallback, err := q.GetCategoryBySlug(ctx, dbgen.GetCategoryBySlugParams{
		Slug: "uncategorised", HouseholdID: &householdID,
	})
	if err != nil {
		return nil, fmt.Errorf("load fallback category: %w", err)
	}

	return &Resolver{
		queries:     q,
		householdID: householdID,
		rules:       compiled,
		fallbackID:  fallback.ID,
	}, nil
}

// Resolve categorises one transaction, returning false when nothing matched
// and the caller should leave it for the AI pass (or the fallback).
func (r *Resolver) Resolve(ctx context.Context, in Input) (Result, bool, error) {
	// 2. Household rules. Highest deterministic priority after a manual choice,
	//    because they are the user stating the answer outright.
	haystack := strings.ToLower(strings.TrimSpace(in.MerchantName + " " + in.Name))
	for _, rule := range r.rules {
		if rule.matches(haystack) {
			return Result{CategoryID: rule.categoryID, Source: SourceRule}, true, nil
		}
	}

	// 3. Merchant cache — a decision already made for this merchant, whether
	//    by hand or by the model.
	if in.MerchantKey != "" {
		categoryID, err := r.queries.LookupMerchantCategory(ctx, dbgen.LookupMerchantCategoryParams{
			HouseholdID: r.householdID, MerchantKey: in.MerchantKey,
		})
		if err == nil {
			return Result{CategoryID: categoryID, Source: SourceCache}, true, nil
		}
		if err != pgx.ErrNoRows {
			return Result{}, false, fmt.Errorf("merchant cache lookup: %w", err)
		}
	}

	// 4. Plaid's own category.
	if in.PFCPrimary != "" {
		var detailed *string
		if in.PFCDetailed != "" {
			detailed = &in.PFCDetailed
		}
		category, err := r.queries.ResolvePFCCategory(ctx, dbgen.ResolvePFCCategoryParams{
			PfcPrimary: in.PFCPrimary, PfcDetailed: detailed,
		})
		if err == nil {
			return Result{CategoryID: category.ID, Source: SourcePlaid}, true, nil
		}
		if err != pgx.ErrNoRows {
			return Result{}, false, fmt.Errorf("pfc lookup: %w", err)
		}
	}

	return Result{}, false, nil
}

// Fallback is the category used when nothing else matched.
func (r *Resolver) Fallback() uuid.UUID { return r.fallbackID }

func (c compiledRule) matches(haystack string) bool {
	switch c.matchType {
	case "equals":
		return haystack == c.lowered
	case "regex":
		return c.regexp != nil && c.regexp.MatchString(haystack)
	default: // "contains"
		return strings.Contains(haystack, c.lowered)
	}
}

// CategoriseHousehold assigns categories to every uncategorised transaction in
// a household, in batches. Safe to re-run: it only touches rows without a
// category, and never a manual one.
func CategoriseHousehold(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, batchSize int32) (int, error) {
	resolver, err := NewResolver(ctx, q, householdID)
	if err != nil {
		return 0, err
	}

	total := 0
	for {
		pending, err := q.ListUncategorisedTransactions(ctx, dbgen.ListUncategorisedTransactionsParams{
			HouseholdID: householdID, Limit: batchSize,
		})
		if err != nil {
			return total, fmt.Errorf("list uncategorised: %w", err)
		}
		if len(pending) == 0 {
			return total, nil
		}

		for _, t := range pending {
			result, ok, err := resolver.Resolve(ctx, Input{
				MerchantKey:  deref(t.MerchantKey),
				MerchantName: deref(t.MerchantName),
				Name:         t.Name,
				PFCPrimary:   deref(t.PlaidPfcPrimary),
				PFCDetailed:  deref(t.PlaidPfcDetailed),
			})
			if err != nil {
				return total, err
			}

			categoryID, source := resolver.Fallback(), string(SourcePlaid)
			if ok {
				categoryID, source = result.CategoryID, string(result.Source)
			}

			if err := q.ApplyCategory(ctx, dbgen.ApplyCategoryParams{
				ID: t.ID, CategoryID: &categoryID, CategorySource: &source,
			}); err != nil {
				return total, fmt.Errorf("apply category: %w", err)
			}
			total++
		}

		// A short final batch means there is nothing left to fetch.
		if int32(len(pending)) < batchSize {
			return total, nil
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
