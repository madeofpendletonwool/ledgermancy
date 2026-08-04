package logos

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/ai"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// passLimit is how many new merchants one pass will consider.
//
// A household's whole descriptor space is a few hundred merchants, so this is
// close to "all of them" on the first pass and reliably zero afterwards — a
// merchant is considered exactly once, ever. The limit exists so the very first
// pass on an imported decade of history does not become one enormous run; the
// remainder is picked up on the next tick.
const passLimit = 200

// batchSize is how many merchants go into one model call. Names are short and
// the answer is a list of domains, so this comfortably fits the 2048-token
// ceiling ResolveMerchantDomains sets while keeping the number of calls low.
const batchSize = 40

// Result reports what a pass did, for the job's log line.
type Result struct {
	Considered int
	Found      int
	NoLogo     int
}

// FetchHousehold resolves and caches logos for every merchant this household
// has not been asked about yet.
//
// Nothing here is retried on a per-merchant failure. A merchant that could not
// be fetched because the host was unreachable simply keeps no row, so the next
// pass picks it up; a merchant with no logo gets a 'none' row and is never
// asked about again. That asymmetry is the point — "we tried and there is
// nothing" is an answer worth caching, "the network was down" is not.
//
// Returns immediately when the household has the preference switched off, so
// the caller does not have to check twice.
func FetchHousehold(
	ctx context.Context,
	q *dbgen.Queries,
	aiClient *ai.Client,
	fetcher *Fetcher,
	householdID uuid.UUID,
	userID uuid.UUID,
) (Result, error) {
	var res Result

	if !HouseholdEnabled(ctx, q, householdID) {
		return res, nil
	}
	if aiClient == nil || !aiClient.Enabled() || fetcher == nil {
		// Config validation makes this unreachable in a running deployment; it
		// is here so a mis-wired caller fails quietly rather than panicking.
		return res, nil
	}

	pending, err := q.ListMerchantsNeedingLogos(ctx, dbgen.ListMerchantsNeedingLogosParams{
		HouseholdID: householdID,
		UserID:      userID,
		RowLimit:    passLimit,
	})
	if err != nil {
		return res, fmt.Errorf("list merchants needing logos: %w", err)
	}
	res.Considered = len(pending)
	if len(pending) == 0 {
		return res, nil
	}

	domains, err := resolveDomains(ctx, aiClient, pending)
	if err != nil {
		// Without domains there is nothing to fetch, and writing 'none' rows off
		// a failed model call would permanently blank merchants over a transient
		// error. Fail the pass instead; the next one starts from the same list.
		return res, fmt.Errorf("resolve merchant domains: %w", err)
	}

	for _, m := range pending {
		domain := domains[m.MerchantKey]
		if domain == "" {
			// The model did not recognise it. That is the requested behaviour —
			// "if it doesn't know then it leaves it" — and it is cached so the
			// same unknown merchant is not re-asked on every pass.
			if err := writeNone(ctx, q, householdID, m, ""); err != nil {
				return res, err
			}
			res.NoLogo++
			continue
		}

		image, contentType, err := fetcher.Fetch(ctx, domain)
		switch {
		case errors.Is(err, ErrNoLogo):
			// A guessed domain that has no logo self-heals here: recorded once,
			// never requested again.
			if err := writeNone(ctx, q, householdID, m, domain); err != nil {
				return res, err
			}
			res.NoLogo++
			continue
		case err != nil:
			// About the host or the network, not about this merchant. Leave no
			// row so the next pass tries again.
			slog.Warn("merchant logo fetch failed",
				"error", err, "household_id", householdID, "domain", domain)
			continue
		}

		if err := q.UpsertMerchantLogo(ctx, dbgen.UpsertMerchantLogoParams{
			HouseholdID:  householdID,
			MerchantKey:  m.MerchantKey,
			MerchantName: m.MerchantName,
			Domain:       &domain,
			ContentType:  &contentType,
			Image:        image,
			State:        "found",
		}); err != nil {
			return res, fmt.Errorf("store merchant logo: %w", err)
		}
		res.Found++
	}

	return res, nil
}

// writeNone records "considered, nothing to show". domain may be empty (the
// model declined) or set (it guessed and the guess had no logo); both are kept
// so a surprising result is explicable later.
func writeNone(
	ctx context.Context,
	q *dbgen.Queries,
	householdID uuid.UUID,
	m dbgen.ListMerchantsNeedingLogosRow,
	domain string,
) error {
	var d *string
	if domain != "" {
		d = &domain
	}
	if err := q.UpsertMerchantLogo(ctx, dbgen.UpsertMerchantLogoParams{
		HouseholdID:  householdID,
		MerchantKey:  m.MerchantKey,
		MerchantName: m.MerchantName,
		Domain:       d,
		State:        "none",
	}); err != nil {
		return fmt.Errorf("store merchant logo miss: %w", err)
	}
	return nil
}

// resolveDomains asks the model about every pending merchant, in batches, and
// returns the merchant keys it answered for mapped to validated domains.
//
// The model is given integer ids and never sees a merchant key, so this owns
// the mapping back. Anything it returns that does not correspond to an id we
// sent, or that is not a bare domain, is dropped — the merchant then falls
// through to a 'none' row, which is the same outcome as the model saying it did
// not know.
func resolveDomains(
	ctx context.Context,
	aiClient *ai.Client,
	pending []dbgen.ListMerchantsNeedingLogosRow,
) (map[string]string, error) {
	out := make(map[string]string, len(pending))

	for start := 0; start < len(pending); start += batchSize {
		end := min(start+batchSize, len(pending))
		batch := pending[start:end]

		req := make([]ai.MerchantForDomain, 0, len(batch))
		for i, m := range batch {
			req = append(req, ai.MerchantForDomain{ID: i, Name: m.MerchantName})
		}

		answers, err := aiClient.ResolveMerchantDomains(ctx, req)
		if err != nil {
			return nil, err
		}

		for _, a := range answers {
			if a.ID < 0 || a.ID >= len(batch) {
				continue
			}
			domain, ok := NormaliseDomain(a.Domain)
			if !ok {
				slog.Warn("discarding unusable merchant domain",
					"merchant", batch[a.ID].MerchantName, "domain", a.Domain)
				continue
			}
			out[batch[a.ID].MerchantKey] = domain
		}
	}

	return out, nil
}
