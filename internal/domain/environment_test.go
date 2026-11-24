package domain

import "testing"

func TestEnvironment_IsProduction(t *testing.T) {
	cases := []struct {
		env  Environment
		want bool
	}{
		{Environment{Type: "production"}, true},
		{Environment{Identifier: "production"}, true},
		{Environment{Type: "production", Identifier: "live"}, true},
		{Environment{Identifier: "dev"}, false},
		{Environment{Type: "development", Identifier: "staging"}, false},
		{Environment{}, false},
	}
	for _, c := range cases {
		if got := c.env.IsProduction(); got != c.want {
			t.Errorf("%+v: IsProduction = %v, want %v", c.env, got, c.want)
		}
	}
}
