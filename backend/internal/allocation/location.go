package allocation

// Asset location — and why it ships as DISCLOSURE rather than as a
// recommendation.
//
// The conventional rules are real: bonds belong in a tax-advantaged account
// because their return is ordinary income taxed annually; equities belong in a
// taxable account because long-term capital gains are taxed lower and losses can
// be harvested; municipal bonds belong in a taxable account because their whole
// point is a federal exemption that is wasted inside a tax shelter.
//
// But every one of those rules rests on a SPREAD the app cannot compute: the gap
// between the household's ordinary-income rate and its long-term capital-gains
// rate. At a 12% marginal rate with a 0% LTCG rate the conventional ordering
// weakens, and in places inverts. Shipping "put your bonds in the 401(k)" as a
// bare recommendation would be this app's first genuinely unsupported claim —
// stated with the same confidence as figures that came out of an exact decimal
// engine.
//
// So this ships in the posture doc 14 took with fee drag: it NAMES the rule, it
// NAMES the assumption the rule rests on, and it says plainly that the app does
// not know the household's bracket. When a marginal-bracket table exists —
// versioned by tax year beside limits.go, keyed by filing_status, ok=false for
// an unconfigured year, owned by whichever doc first needs it — this computes
// the spread and the disclosure becomes a figure. Until then it does not
// pretend to.

// LocationRule is one asset-class-to-account-type rule with its reasoning and
// its assumption attached. The assumption is not a footnote: it is the reason
// the rule might not apply to the household reading it.
type LocationRule struct {
	AssetClass string `json:"asset_class"`
	// PreferredAccount is the account TYPE the rule points at, in the vocabulary
	// accounts.tax_treatment uses.
	PreferredAccount string `json:"preferred_account"`
	Reason           string `json:"reason"`
	Assumption       string `json:"assumption"`
}

// AssetLocationDisclosure is the whole answer: the rules, and the reason none of
// them is being called a recommendation.
type AssetLocationDisclosure struct {
	Rules []LocationRule `json:"rules"`
	// IsRecommendation is false, always, and it is in the payload so a client
	// cannot render these rules as advice by accident.
	IsRecommendation bool   `json:"is_recommendation"`
	BracketKnown     bool   `json:"bracket_known"`
	Note             string `json:"note"`
}

// AssetLocation returns the disclosure. It takes no arguments because it depends
// on nothing the app knows — which is precisely the point being made.
func AssetLocation() AssetLocationDisclosure {
	const spreadAssumption = "Assumes your ordinary-income rate exceeds your long-term capital-gains rate — " +
		"true for most households above the 22% bracket, and not true for every household."

	return AssetLocationDisclosure{
		IsRecommendation: false,
		BracketKnown:     false,
		Note: "These are the conventional asset-location rules, stated with the assumption each one rests on. " +
			"Ledgermancy does not know your marginal tax bracket, so it cannot tell you whether the assumption " +
			"holds for you — and the rules can weaken or invert when it does not. Treat this as background, " +
			"not as a recommendation.",
		Rules: []LocationRule{
			{
				AssetClass:       "Bonds and bond funds",
				PreferredAccount: "trad_401k / trad_ira / roth_ira",
				Reason: "Interest is ordinary income and is taxed every year it is earned, so sheltering it " +
					"defers or removes the largest annual tax bill in a portfolio.",
				Assumption: spreadAssumption,
			},
			{
				AssetClass:       "Equities and equity funds",
				PreferredAccount: "taxable",
				Reason: "Long-term gains are taxed at a lower rate than ordinary income, the tax is deferred until " +
					"you sell, losses can be harvested against other gains, and heirs get a stepped-up basis. " +
					"None of those advantages survive inside a tax-deferred account.",
				Assumption: spreadAssumption,
			},
			{
				AssetClass:       "Municipal / tax-exempt bonds",
				PreferredAccount: "taxable",
				Reason: "The entire value of a municipal bond is its federal tax exemption. Holding one inside a " +
					"tax-advantaged account wastes the exemption and accepts the lower yield that pays for it.",
				Assumption: "Assumes you are paying enough federal tax for the exemption to be worth its lower yield. " +
					"Below roughly the 22% bracket a taxable bond often nets more.",
			},
			{
				AssetClass:       "REITs and high-turnover funds",
				PreferredAccount: "trad_401k / trad_ira / roth_ira",
				Reason: "REIT distributions are largely ordinary income and high-turnover funds throw off " +
					"short-term gains, both taxed at your ordinary rate in a taxable account.",
				Assumption: spreadAssumption,
			},
			{
				AssetClass:       "Highest expected-return holdings",
				PreferredAccount: "roth_ira / roth_401k",
				Reason: "Roth growth is never taxed, so the account with the most growth in it is the one worth " +
					"sheltering permanently.",
				Assumption: "Assumes your tax rate in retirement is not materially lower than it is today. " +
					"If it will be much lower, a traditional account may come out ahead instead.",
			},
		},
	}
}
