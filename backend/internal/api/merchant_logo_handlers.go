package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/logos"
)

// logoCacheSeconds is how long a browser may keep a merchant logo.
//
// A day, and `private` so a shared proxy never holds it: the bytes are public
// brand imagery, but the fact that this household asked for THIS merchant is
// not. Long is safe because the answer genuinely does not change — a merchant's
// logo is fetched once and never refreshed, so a stale cache is a correct one.
const logoCacheSeconds = 86400

// handleMerchantLogo serves a cached merchant logo from this app's own origin.
//
// 404 is the ordinary answer and the frontend is built around it: the feature
// switched off, the household opted out, the merchant never resolved, or it
// resolved to nothing all produce the same response, and the avatar falls back
// to its monogram in every one of them. Nothing here reveals which.
//
// Note what this endpoint is NOT: a proxy. It never contacts Logo.dev. If the
// worker has not cached a logo, the answer is 404, not a fetch on the request
// path — that is what keeps a page render from depending on a third party.
func (s *Server) handleMerchantLogo(w http.ResponseWriter, r *http.Request) {
	if !s.Config.MerchantLogos.Ready(s.Config.AI) {
		http.NotFound(w, r)
		return
	}

	identity := auth.MustFromContext(r.Context())

	key := r.URL.Query().Get("key")
	if key == "" {
		http.NotFound(w, r)
		return
	}

	// The household's own switch, checked on the read path too: turning logos
	// off should stop showing them immediately, without waiting for the cache
	// to be cleared.
	if !logos.HouseholdEnabled(r.Context(), s.Queries, identity.HouseholdID) {
		http.NotFound(w, r)
		return
	}

	row, err := s.Queries.GetMerchantLogo(r.Context(), dbgen.GetMerchantLogoParams{
		HouseholdID: identity.HouseholdID,
		MerchantKey: key,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.internalError(w, "get merchant logo", err)
			return
		}
		http.NotFound(w, r)
		return
	}

	// Sniffed from the bytes, never from the stored column — the same rule the
	// document vault's download follows, and for the same reason: these bytes
	// are about to be rendered on this app's origin.
	contentType, ok := logos.ServedContentType(row.Image)
	if !ok {
		slog.Error("cached merchant logo is not a servable image",
			"household_id", identity.HouseholdID, "merchant_key", key)
		http.NotFound(w, r)
		return
	}

	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(row.Image)))
	h.Set("Cache-Control", "private, max-age="+strconv.Itoa(logoCacheSeconds))
	// Set globally by the middleware; repeated here because this route hands a
	// browser image bytes and losing it is what makes sniffing matter.
	h.Set("X-Content-Type-Options", "nosniff")

	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(row.Image); err != nil {
		slog.Warn("merchant logo write interrupted", "merchant_key", key, "error", err)
	}
}
