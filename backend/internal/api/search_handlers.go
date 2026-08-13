package api

import (
	"net/http"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/search"
)

// handleSearchOperators lists the operator vocabulary the transaction search bar
// accepts, so the autocomplete is generated from the parser rather than kept in
// step with it by hand.
//
// A static list in the frontend would be one more thing to remember to edit, and
// the failure is silent: the box suggests an operator the parser has never heard
// of and the user's query quietly degrades to free text. Serving it means the
// parser is the only place the vocabulary is written down — which matters more
// once the rules engine offers the same operators for its triggers.
//
// The response is the same for every caller and changes only when the binary
// does, so it is safe for the client to fetch once and keep.
func (s *Server) handleSearchOperators(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, search.Operators())
}
