package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/documents"
)

// The receipt-to-transaction match, and specifically the case the first cut of
// this feature got wrong: a receipt scanned before its charge posts.
//
// Matching used to run exactly once, inside the extraction request. With the
// charge not yet in the ledger there was nothing to match, nothing was stored,
// and no later pass looked again — so the normal way anyone handles a receipt
// was the way that failed. These tests pin the fix: the reading is cached, and
// the match re-runs against it for free.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/api/
func TestReceiptMatching(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// The match endpoint never touches a blob — it compares stored fields
	// against the ledger — so a vault with no cipher or storage behind it is
	// enough to get past the "is the vault configured" guard, and proves the
	// endpoint really does no decryption.
	srv := &Server{
		Pool:      pool,
		Queries:   dbgen.New(pool),
		Documents: documents.New(config.DocumentsConfig{Enabled: true}, nil, nil),
	}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}

	household, user := uuid.New(), uuid.New()
	itemID, accountID := uuid.New(), uuid.New()

	exec(`INSERT INTO households (id, name) VALUES ($1, 'Receipt Match')`, household)
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Tester')`, user, household, user.String()+"@example.test")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, institution_name, is_shared)
	      VALUES ($1, $2, $3, '\x00'::bytea, 'Test Bank', TRUE)`, itemID, user, itemID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, is_active)
	      VALUES ($1, $2, $3, 'Card', 'credit', TRUE)`, accountID, itemID, accountID.String())

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, household)
	})

	caller := auth.Identity{UserID: user, HouseholdID: household, DisplayName: "Tester"}
	withDoc := func(r *http.Request, id uuid.UUID) *http.Request {
		routeCtx := chi.NewRouteContext()
		routeCtx.URLParams.Add("documentID", id.String())
		return r.WithContext(context.WithValue(
			auth.ContextWithIdentity(ctx, caller), chi.RouteCtxKey, routeCtx))
	}

	// A receipt already read by OCR, as the extraction endpoint would have left
	// it: fields cached on the row, no transaction to attach to yet.
	swipedOn := time.Date(2026, time.July, 4, 0, 0, 0, 0, time.UTC)
	receiptID := uuid.New()
	seedReceipt := func(id uuid.UUID, amount string, shared bool) {
		exec(`INSERT INTO documents
		      (id, household_id, uploaded_by, is_shared, title, doc_type, filename,
		       mime_type, size_bytes, storage_key, content_hash,
		       extracted_at, extracted_merchant, extracted_amount, extracted_date, extracted_confidence)
		      VALUES ($1, $2, $3, $4, 'Costco run', 'receipt', 'receipt.png',
		              'image/png', 1024, $5, $6,
		              now(), 'Costco', $7, $8, 0.9)`,
			id, household, user, shared,
			documents.NewStorageKey(), "hash-"+id.String(), amount, swipedOn)
	}
	seedReceipt(receiptID, "84.20", true)

	matchesFor := func(t *testing.T, id uuid.UUID) []receiptMatchResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.handleDocumentMatches(rec, withDoc(httptest.NewRequest(http.MethodGet, "/m", nil), id))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
		}
		var out []receiptMatchResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	t.Run("no match before the charge posts", func(t *testing.T) {
		if got := matchesFor(t, receiptID); len(got) != 0 {
			t.Fatalf("got %d matches before anything posted, want 0", len(got))
		}
	})

	t.Run("a pending charge is not offered", func(t *testing.T) {
		// Pending rows are deleted when the posted one arrives, and
		// document_links cascades — so a link to one would silently vanish.
		pendingID := uuid.New()
		exec(`INSERT INTO transactions
		      (id, account_id, plaid_transaction_id, amount, date, authorized_date, name, pending)
		      VALUES ($1, $2, $3, '84.20', '2026-07-05', '2026-07-04', 'COSTCO WHSE', TRUE)`,
			pendingID, accountID, pendingID.String())
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM transactions WHERE id = $1`, pendingID)
		})

		if got := matchesFor(t, receiptID); len(got) != 0 {
			t.Fatalf("a pending transaction was offered as a match: %+v", got)
		}
	})

	// The case that used to be impossible. The charge posts three days after the
	// receipt was scanned, and the match is found with no second OCR run.
	t.Run("the charge posts days later and is found", func(t *testing.T) {
		postedID := uuid.New()
		exec(`INSERT INTO transactions
		      (id, account_id, plaid_transaction_id, amount, date, authorized_date, name, merchant_name, pending)
		      VALUES ($1, $2, $3, '84.20', '2026-07-07', '2026-07-04', 'COSTCO WHSE 1234', 'Costco', FALSE)`,
			postedID, accountID, postedID.String())
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM transactions WHERE id = $1`, postedID)
		})

		got := matchesFor(t, receiptID)
		if len(got) != 1 {
			t.Fatalf("got %d matches, want 1: %+v", len(got), got)
		}
		if got[0].TransactionID != postedID {
			t.Errorf("matched the wrong transaction")
		}
		// The posted date is three days off the receipt; the authorisation is
		// exact. Both travel back so the UI can explain the gap.
		if got[0].Date != "2026-07-07" {
			t.Errorf("date = %q, want the posted date", got[0].Date)
		}
		if got[0].AuthorizedDate == nil || *got[0].AuthorizedDate != "2026-07-04" {
			t.Errorf("authorized_date = %v, want 2026-07-04", got[0].AuthorizedDate)
		}
	})

	// A charge whose posted date is outside the window but whose authorisation
	// is inside it must still match — the receipt was printed on the swipe date.
	t.Run("an authorisation inside the window matches a late posting", func(t *testing.T) {
		lateID := uuid.New()
		exec(`INSERT INTO transactions
		      (id, account_id, plaid_transaction_id, amount, date, authorized_date, name, pending)
		      VALUES ($1, $2, $3, '55.00', '2026-07-20', '2026-07-04', 'SLOW MERCHANT', FALSE)`,
			lateID, accountID, lateID.String())
		lateReceipt := uuid.New()
		seedReceipt(lateReceipt, "55.00", true)
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM transactions WHERE id = $1`, lateID)
			_, _ = pool.Exec(context.Background(), `DELETE FROM documents WHERE id = $1`, lateReceipt)
		})

		got := matchesFor(t, lateReceipt)
		if len(got) != 1 || got[0].TransactionID != lateID {
			t.Fatalf("a charge posting 16 days late but authorised on the receipt's date was not matched: %+v", got)
		}
	})

	t.Run("attaching removes it from the awaiting-match set", func(t *testing.T) {
		before, err := srv.Queries.ListReceiptsAwaitingMatch(ctx, dbgen.ListReceiptsAwaitingMatchParams{
			HouseholdID: household,
			Since:       time.Now().UTC().AddDate(0, 0, -45),
		})
		if err != nil {
			t.Fatalf("awaiting: %v", err)
		}
		found := false
		for _, row := range before {
			if row.ID == receiptID {
				found = true
			}
		}
		if !found {
			t.Fatal("an unattached, already-read receipt is not in the awaiting-match set")
		}

		txID := uuid.New()
		exec(`INSERT INTO transactions
		      (id, account_id, plaid_transaction_id, amount, date, name, pending)
		      VALUES ($1, $2, $3, '84.20', '2026-07-07', 'COSTCO', FALSE)`,
			txID, accountID, txID.String())
		exec(`INSERT INTO document_links (document_id, transaction_id) VALUES ($1, $2)`,
			receiptID, txID)

		after, err := srv.Queries.ListReceiptsAwaitingMatch(ctx, dbgen.ListReceiptsAwaitingMatchParams{
			HouseholdID: household,
			Since:       time.Now().UTC().AddDate(0, 0, -45),
		})
		if err != nil {
			t.Fatalf("awaiting: %v", err)
		}
		for _, row := range after {
			if row.ID == receiptID {
				t.Error("an attached receipt is still awaiting a match; the nudge would repeat forever")
			}
		}
	})

	t.Run("a private receipt never reaches the household feed", func(t *testing.T) {
		privateID := uuid.New()
		seedReceipt(privateID, "12.34", false)
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM documents WHERE id = $1`, privateID)
		})

		rows, err := srv.Queries.ListReceiptsAwaitingMatch(ctx, dbgen.ListReceiptsAwaitingMatchParams{
			HouseholdID: household,
			Since:       time.Now().UTC().AddDate(0, 0, -45),
		})
		if err != nil {
			t.Fatalf("awaiting: %v", err)
		}
		for _, row := range rows {
			if row.ID == privateID {
				t.Error("a private receipt is in the feed's working set")
			}
		}
	})
}
