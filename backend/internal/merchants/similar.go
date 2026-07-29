// Package merchants canonicalises fragmented bank descriptors into merchant
// entities.
//
// plaid.MerchantKey already collapses store numbers, processor prefixes and a
// city/state tail into a stable key. That is the right primitive: it is a pure
// string transform, so it is cheap, deterministic and has no state to get out of
// date. It is also the wrong unit of analysis, because a pure transform has no
// memory — it cannot know that AMAZON.COM, AMZN.COM/BILL and AMZ*ORDER are one
// business.
//
// This package supplies the memory. Nothing here changes MerchantKey; it layers
// on top of it.
package merchants

import "strings"

// Descriptor noise. These tokens appear across unrelated merchants — they are
// how a bank says "this was a card payment on a website", not what the business
// is called. Comparing on them is how you conclude that two random online
// merchants are the same company, so they are dropped before any comparison.
//
// Kept deliberately short. Every entry here is a word that carries no brand
// signal at all; anything arguable ("market", "cafe", "shop") stays in, because
// dropping a word that DOES distinguish two businesses causes a wrong merge,
// which is the expensive direction of this trade.
var descriptorNoise = map[string]struct{}{
	"com": {}, "www": {}, "net": {}, "org": {}, "http": {}, "https": {},
	"inc": {}, "llc": {}, "ltd": {}, "corp": {}, "co": {}, "company": {},
	"the": {}, "and": {},
	"bill": {}, "billing": {}, "payment": {}, "payments": {}, "pay": {},
	"purchase": {}, "order": {}, "orders": {}, "online": {}, "web": {},
	"usa": {}, "us": {},
}

// Confidence scores for each rule, ordered by how much evidence the rule
// actually has. They are recorded on the suggested alias and shown in the review
// UI so a person can weight a borderline proposal — they are never a threshold
// for acting automatically, because nothing here acts automatically.
const (
	confIdentical  = 0.95 // same significant tokens
	confContained  = 0.80 // one descriptor is the other plus extra tokens
	confAbbreviated = 0.70 // vowel-dropped or truncated brand token
)

// significantTokens strips descriptor noise and returns what is left, in order.
// A token of one character is dropped too: it is a fragment, not a word, and it
// matches far too much.
//
// Returns nil when nothing survives. A key with no significant tokens (a bare
// "BILL PAYMENT") is not a merchant name and must not be compared against
// anything — every caller checks for that.
func significantTokens(key string) []string {
	fields := strings.Fields(key)
	out := make([]string, 0, len(fields))
	for _, tok := range fields {
		if len(tok) < 2 {
			continue
		}
		if _, noise := descriptorNoise[tok]; noise {
			continue
		}
		out = append(out, tok)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Similarity is the verdict on one pair of merchant keys.
type Similarity struct {
	Match bool
	// Confidence is one of the conf* constants; 0 when Match is false.
	Confidence float64
	// Reason names the rule that fired, for the review UI. A user deciding on a
	// merge is much better served by "AMZN is an abbreviation of AMAZON" than by
	// a bare number.
	Reason string
}

// Compare decides whether two merchant keys look like the same business.
//
// The rules are ordered from most to least evidence, and every one of them is
// conservative by design: this produces *suggestions* a person reviews, and the
// cost of the two errors is wildly asymmetric. A missed merge leaves the app
// behaving exactly as it does today. A wrong merge silently rewrites spending
// history into a shape the user cannot explain.
//
// The canonical negative case is "blue bottle" vs "blue ridge": two genuinely
// different businesses sharing a leading token. Nothing below fires on it —
// neither is contained in the other, and their distinguishing tokens fail the
// abbreviation test on the very first character pair.
func Compare(keyA, keyB string) Similarity {
	if keyA == keyB {
		return Similarity{}
	}
	a, b := significantTokens(keyA), significantTokens(keyB)
	if a == nil || b == nil {
		return Similarity{}
	}

	// 1. The same significant tokens in the same order. The keys differed only
	//    by noise ("netflix" vs "netflix com").
	if equalTokens(a, b) {
		return Similarity{Match: true, Confidence: confIdentical, Reason: "same merchant name once descriptor noise is removed"}
	}

	// 2. One is the other plus trailing junk ("amazon com" vs
	//    "amazon com abcd1"). Anchored at the front: a descriptor leads with the
	//    brand, so a shared PREFIX of tokens is evidence and a shared suffix is
	//    usually just a shared shopping-mall or city word.
	if short, long, ok := prefixContained(a, b); ok {
		return Similarity{
			Match:      true,
			Confidence: confContained,
			Reason:     "\"" + strings.Join(long, " ") + "\" starts with \"" + strings.Join(short, " ") + "\"",
		}
	}

	// 3. Abbreviation of the brand token ("amz"/"amzn" vs "amazon"). Compared on
	//    the FIRST significant token only: that is where the brand lives, and
	//    running this rule over every token pair is how "ridge" ends up matching
	//    something it should not.
	if abbreviates(a[0], b[0]) {
		return Similarity{
			Match:      true,
			Confidence: confAbbreviated,
			Reason:     "\"" + shorterOf(a[0], b[0]) + "\" reads as an abbreviation of \"" + longerOf(a[0], b[0]) + "\"",
		}
	}

	return Similarity{}
}

func equalTokens(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// prefixContained reports whether the shorter token list is a leading run of the
// longer one. Equal-length lists are handled by rule 1 and never reach here.
func prefixContained(a, b []string) (short, long []string, ok bool) {
	short, long = a, b
	if len(b) < len(a) {
		short, long = b, a
	}
	if len(short) == len(long) {
		return nil, nil, false
	}
	for i := range short {
		if short[i] != long[i] {
			return nil, nil, false
		}
	}
	return short, long, true
}

// abbreviates reports whether one token is a plausible abbreviation of the
// other — the vowel-dropping banks and merchants do constantly ("AMZN" for
// "AMAZON", "MSFT" for "MICROSOFT").
//
// Three guards keep this from becoming a wildcard. Subsequence matching alone is
// far too loose: over a large descriptor set, most short tokens are a
// subsequence of some longer unrelated one.
//
//   - Both tokens share their first two characters. Abbreviations keep the
//     opening of the word; this is what rules out "blue"/"bottle" ("bl"/"bo")
//     and the overwhelming majority of coincidental subsequences.
//   - The short token is at least three characters. Two-character fragments
//     match nearly everything.
//   - The long token is at most 2.5x the short one. "amz" abbreviating "amazon"
//     is a 2x contraction; a 5x one is not an abbreviation, it is a coincidence.
func abbreviates(x, y string) bool {
	short, long := x, y
	if len(y) < len(x) {
		short, long = y, x
	}
	if short == long || len(short) < 3 {
		return false
	}
	if short[:2] != long[:2] {
		return false
	}
	if len(long)*2 > len(short)*5 {
		return false
	}
	return isSubsequence(short, long)
}

// isSubsequence reports whether every character of short appears in long, in
// order. Dropping interior vowels is the abbreviation this models.
func isSubsequence(short, long string) bool {
	i := 0
	for j := 0; j < len(long) && i < len(short); j++ {
		if short[i] == long[j] {
			i++
		}
	}
	return i == len(short)
}

func shorterOf(a, b string) string {
	if len(b) < len(a) {
		return b
	}
	return a
}

func longerOf(a, b string) string {
	if len(b) < len(a) {
		return a
	}
	return b
}

// DisplayName turns a merchant key into a presentable canonical name:
// "amazon com" becomes "Amazon". It is the default the review UI offers, not a
// decision — an entity's name is freely renamed afterwards.
//
// Noise tokens are dropped for the same reason they are dropped for comparison:
// "Amazon Com" is not what the business is called.
func DisplayName(key string) string {
	tokens := significantTokens(key)
	if tokens == nil {
		tokens = strings.Fields(key)
	}
	if len(tokens) == 0 {
		return key
	}
	out := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		out = append(out, strings.ToUpper(tok[:1])+tok[1:])
	}
	return strings.Join(out, " ")
}
