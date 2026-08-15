package config

import "testing"

// ChatModels is what the Advisor chat's selector is built from, and the two
// properties it has to keep are the ones a selector silently breaks on: the
// PRIMARY stays first (a client defaulting to the head of the list must end up
// on the primary, not on whatever an operator happened to list first), and a
// duplicate entry must not produce a selector with the same model twice —
// which is exactly what copying a list out of a provider's docs produces.
func TestChatModels(t *testing.T) {
	cases := []struct {
		name string
		cfg  AIConfig
		want []string
	}{
		{
			name: "no additional models is a one-entry list, not empty",
			cfg:  AIConfig{Model: "glm-4.6"},
			want: []string{"glm-4.6"},
		},
		{
			name: "additional models follow the primary in order",
			cfg:  AIConfig{Model: "glm-4.6", AdditionalModels: []string{"claude-sonnet-4-5", "glm-4.5-air"}},
			want: []string{"glm-4.6", "claude-sonnet-4-5", "glm-4.5-air"},
		},
		{
			name: "the primary repeated in the additional list is dropped",
			cfg:  AIConfig{Model: "glm-4.6", AdditionalModels: []string{"glm-4.6", "claude-sonnet-4-5"}},
			want: []string{"glm-4.6", "claude-sonnet-4-5"},
		},
		{
			name: "a duplicate additional entry appears once",
			cfg:  AIConfig{Model: "glm-4.6", AdditionalModels: []string{"claude-sonnet-4-5", "claude-sonnet-4-5"}},
			want: []string{"glm-4.6", "claude-sonnet-4-5"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.ChatModels()
			if len(got) != len(tc.want) {
				t.Fatalf("ChatModels() = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("ChatModels() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
