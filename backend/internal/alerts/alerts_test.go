package alerts

import (
	"testing"
	"time"
)

func TestValidateConfig(t *testing.T) {
	cases := []struct {
		name      string
		alertType string
		config    string
		wantErr   bool
	}{
		{"big_spend ok", TypeBigSpend, `{"threshold":"200.00"}`, false},
		{"big_spend zero", TypeBigSpend, `{"threshold":"0"}`, true},
		{"big_spend missing", TypeBigSpend, `{}`, true},
		{"big_spend bad decimal", TypeBigSpend, `{"threshold":"abc"}`, true},
		{"budget ok", TypeBudgetThreshold, `{"percent":90}`, false},
		{"budget zero", TypeBudgetThreshold, `{"percent":0}`, true},
		{"budget over range", TypeBudgetThreshold, `{"percent":5000}`, true},
		{"unusual ok", TypeUnusualMerchant, `{"recent_days":7,"min_amount":"50.00"}`, false},
		{"unusual defaults ok", TypeUnusualMerchant, `{}`, false},
		{"unusual negative", TypeUnusualMerchant, `{"recent_days":-1}`, true},
		{"low_leftover ok", TypeLowLeftover, `{"floor":"500.00"}`, false},
		{"low_leftover zero ok", TypeLowLeftover, `{"floor":"0"}`, false},
		{"low_leftover negative", TypeLowLeftover, `{"floor":"-1"}`, true},
		{"unknown type", "nonsense", `{}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateConfig(tc.alertType, []byte(tc.config))
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateConfig(%s,%s) err=%v, wantErr=%v", tc.alertType, tc.config, err, tc.wantErr)
			}
		})
	}
}

func TestIsValidType(t *testing.T) {
	for _, ty := range Types {
		if !IsValidType(ty) {
			t.Errorf("Types contains %q but IsValidType says false", ty)
		}
	}
	if IsValidType("made_up") {
		t.Error("IsValidType accepted an unknown type")
	}
}

func TestMonthBounds(t *testing.T) {
	// A day mid-month in a leap February should bound to Feb 1 .. Feb 29.
	now := time.Date(2024, time.February, 14, 9, 30, 0, 0, time.UTC)
	from, to := monthBounds(now)
	if from.Format(time.DateOnly) != "2024-02-01" {
		t.Errorf("from = %s", from.Format(time.DateOnly))
	}
	if to.Format(time.DateOnly) != "2024-02-29" {
		t.Errorf("to = %s", to.Format(time.DateOnly))
	}
}
