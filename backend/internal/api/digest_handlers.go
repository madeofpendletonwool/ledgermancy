package api

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// Page sizes for the digest history. A household produces one entry per period,
// so a page of twelve is roughly a quarter of weekly digests or a year of
// monthly ones — enough that most people never page at all.
const (
	digestPageDefault = 12
	digestPageMax     = 50
)

// digestEntryResponse is one stored digest.
//
// payload is passed through as raw JSON rather than re-marshalled from a Go
// struct. That is deliberate and is the immutability guarantee reaching the
// wire: the client receives exactly the bytes that were frozen when the digest
// was generated, so a later change to the payload struct cannot retroactively
// alter what an old digest says.
type digestEntryResponse struct {
	ID          uuid.UUID       `json:"id"`
	Cadence     string          `json:"cadence"`
	PeriodKey   string          `json:"period_key"`
	PeriodStart string          `json:"period_start"`
	PeriodEnd   string          `json:"period_end"`
	Label       string          `json:"label"`
	Payload     json.RawMessage `json:"payload"`
	Narrative   *string         `json:"narrative"`
	ReadAt      *string         `json:"read_at"`
	CreatedAt   string          `json:"created_at"`
}

// digestListResponse wraps the page with the counts a client needs to page and
// to badge, so neither costs a second request.
type digestListResponse struct {
	Entries []digestEntryResponse `json:"entries"`
	Total   int64                 `json:"total"`
	Unread  int64                 `json:"unread"`
	Limit   int                   `json:"limit"`
	Offset  int                   `json:"offset"`
}

func toDigestEntryResponse(e dbgen.DigestEntry) digestEntryResponse {
	resp := digestEntryResponse{
		ID:          e.ID,
		Cadence:     e.Cadence,
		PeriodKey:   e.PeriodKey,
		PeriodStart: e.PeriodStart.Format(time.DateOnly),
		PeriodEnd:   e.PeriodEnd.Format(time.DateOnly),
		Label:       e.Label,
		Payload:     json.RawMessage(e.Payload),
		Narrative:   e.Narrative,
		CreatedAt:   e.CreatedAt.UTC().Format(time.RFC3339),
	}
	if e.ReadAt != nil {
		s := e.ReadAt.UTC().Format(time.RFC3339)
		resp.ReadAt = &s
	}
	return resp
}

// handleListDigests serves the caller's digest history, newest first.
//
// Scoped to the requesting user, never to the household. A digest entry is one
// member's view of the money, computed under their visibility — another member
// reading it would hand them figures that include institutions deliberately not
// shared with them.
func (s *Server) handleListDigests(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	limit := clampQueryInt(r, "limit", digestPageDefault, 1, digestPageMax)
	offset := clampQueryInt(r, "offset", 0, 0, 1<<20)

	rows, err := s.Queries.ListDigestEntries(r.Context(), dbgen.ListDigestEntriesParams{
		UserID: identity.UserID,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		s.internalError(w, "list digests", err)
		return
	}
	counts, err := s.Queries.CountDigestEntries(r.Context(), identity.UserID)
	if err != nil {
		s.internalError(w, "count digests", err)
		return
	}

	entries := make([]digestEntryResponse, 0, len(rows))
	for _, e := range rows {
		entries = append(entries, toDigestEntryResponse(e))
	}
	writeJSON(w, http.StatusOK, digestListResponse{
		Entries: entries,
		Total:   counts.Total,
		Unread:  counts.Unread,
		Limit:   int(limit),
		Offset:  int(offset),
	})
}

// handleGetDigest serves one entry by id, for a deep link or an entry older than
// the loaded page. 404 for an id that is not the caller's — the query scopes on
// user_id, so a foreign id is indistinguishable from a missing one, which is the
// right answer to give.
func (s *Server) handleGetDigest(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	id, err := uuid.Parse(chi.URLParam(r, "digestID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid digest id")
		return
	}

	entry, err := s.Queries.GetDigestEntry(r.Context(), dbgen.GetDigestEntryParams{
		ID: id, UserID: identity.UserID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "digest not found")
		return
	}
	if err != nil {
		s.internalError(w, "get digest", err)
		return
	}
	writeJSON(w, http.StatusOK, toDigestEntryResponse(entry))
}

// handleMarkDigestRead stamps read_at, once. Mirrors the insight mark-read: a
// no-op for an id that is not the caller's, and 204 either way, because "did
// that id exist" is not information a read receipt should leak.
func (s *Server) handleMarkDigestRead(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	id, err := uuid.Parse(chi.URLParam(r, "digestID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid digest id")
		return
	}

	if err := s.Queries.MarkDigestEntryRead(r.Context(), dbgen.MarkDigestEntryReadParams{
		ID: id, UserID: identity.UserID,
	}); err != nil {
		s.internalError(w, "mark digest read", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// clampQueryInt reads an integer query parameter, falling back to a default on
// anything unparseable and clamping into range. A bad page size is a client bug
// worth ignoring, not worth a 400 that breaks the page.
func clampQueryInt(r *http.Request, key string, fallback, min, max int32) int32 {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	if n < int(min) {
		return min
	}
	if n > int(max) {
		return max
	}
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(n)
}
