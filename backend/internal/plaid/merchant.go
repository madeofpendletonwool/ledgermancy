package plaid

import (
	"regexp"
	"strings"
	"unicode"
)

// Merchant strings from banks carry a lot of noise: store numbers, cities,
// dates, reference ids, and payment-processor prefixes. These strip that down
// to a stable key so "SQ *BLUE BOTTLE #4412 OAKLAND CA" and
// "SQ *BLUE BOTTLE #0087 BERKELEY CA" resolve to the same merchant, letting
// one categorization decision cover both.
//
// The goal is stability, not a pretty name: the key only has to be identical
// across repeat visits to the same merchant, and distinct between merchants.
var (
	// Payment processor / channel prefixes.
	merchantPrefixes = regexp.MustCompile(`(?i)^(sq \*|tst\*|sp \*|pp\*|paypal \*|pos |purchase |payment |recurring |ach |debit card |visa |mc )+`)

	// Trailing store numbers, e.g. "#4412" or "STORE 0087".
	merchantStoreNum = regexp.MustCompile(`(?i)\s*(#\s*\d+|store\s+\d+|\bno\.?\s*\d+)\b`)

	// A trailing "CITY ST" tail, e.g. "OAKLAND CA".
	//
	// Only ONE word before the state code is consumed. Matching more was a bug:
	// a greedy city pattern turned "WHOLE FOODS MKT AUSTIN TX" into "whole",
	// which would collide with every other merchant starting with that word.
	// A two-word city like "SAN FRANCISCO CA" therefore leaves a stray "san",
	// which is harmless — it is still stable and still merchant-specific.
	merchantLocation = regexp.MustCompile(`(?i)\s+[a-z][a-z'-]+\s+(a[klrz]|ar|az|c[aot]|d[ce]|fl|ga|hi|i[adln]|k[sy]|la|m[adeinost]|n[cdehjmvy]|o[hkr]|pa|ri|s[cd]|t[nx]|ut|v[at]|w[aivy])$`)

	merchantPunct = regexp.MustCompile(`[^\p{L}\p{N}\s&'-]+`)
	merchantSpace = regexp.MustCompile(`\s+`)
)

// MerchantKey normalizes a merchant name or transaction description into a
// stable lookup key. It returns "" when nothing meaningful survives, in which
// case the caller should not attempt a cache lookup.
//
// Plaid's own merchant_name is already clean when present, so callers should
// prefer it and fall back to the raw transaction name.
func MerchantKey(merchantName, transactionName string) string {
	source := merchantName
	if strings.TrimSpace(source) == "" {
		source = transactionName
	}

	key := strings.ToLower(strings.TrimSpace(source))
	if key == "" {
		return ""
	}

	key = merchantPrefixes.ReplaceAllString(key, "")
	key = merchantStoreNum.ReplaceAllString(key, "")

	// Strip the location tail only if something is left afterwards, so a
	// merchant name that happens to look like "CITY ST" is not erased.
	if stripped := merchantLocation.ReplaceAllString(key, ""); strings.TrimSpace(stripped) != "" {
		key = stripped
	}

	key = merchantPunct.ReplaceAllString(key, " ")
	key = dropReferenceTokens(key)
	key = merchantSpace.ReplaceAllString(key, " ")
	key = strings.TrimSpace(key)

	// A key of one or two characters is noise, not a merchant.
	if len(key) < 3 {
		return ""
	}
	return key
}

// dropReferenceTokens removes order and reference identifiers, which differ on
// every transaction and would otherwise make each visit to the same merchant
// look unique — defeating the categorization cache entirely.
//
// A token is treated as a reference id when it is either all digits (3+), or
// mixes letters with two or more digits (e.g. "8H47DK2"). Requiring two digits
// deliberately spares real names that merely contain one, such as "7eleven".
func dropReferenceTokens(s string) string {
	fields := strings.Fields(s)
	kept := make([]string, 0, len(fields))

	for _, token := range fields {
		var digits, letters int
		for _, r := range token {
			switch {
			case unicode.IsDigit(r):
				digits++
			case unicode.IsLetter(r):
				letters++
			}
		}

		isNumericRef := letters == 0 && digits >= 3
		isMixedRef := letters > 0 && digits >= 2
		if isNumericRef || isMixedRef {
			continue
		}
		kept = append(kept, token)
	}

	return strings.Join(kept, " ")
}
