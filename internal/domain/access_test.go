package domain

import "testing"

// TestAnUnknownPolicyFailsClosed: the edge refuses rather than guesses. A
// route built without a policy, or with one from a newer build, keeps its
// members-only pages for members.
func TestAnUnknownPolicyFailsClosed(t *testing.T) {
	for _, a := range []Access{{}, {Mode: "secret"}} {
		if !a.MembersOnly("/f/contact") {
			t.Errorf("%+v let a stranger in", a)
		}
	}
	if (Access{Mode: AccessPublic}).MembersOnly("/admin") {
		t.Error("a public app kept a page for members")
	}
}

func TestAccessValidation(t *testing.T) {
	for _, tc := range []struct {
		access Access
		ok     bool
	}{
		{Access{Mode: AccessPublic}, true},
		{Access{Mode: AccessInternal}, true},
		{Access{Mode: AccessHybrid, Public: []string{"/f/"}}, true},
		{Access{Mode: AccessHybrid}, false},
		{Access{Mode: AccessHybrid, Public: []string{"f/"}}, false},
		{Access{Mode: AccessHybrid, Public: []string{"//evil/"}}, false},
		{Access{Mode: AccessHybrid, Public: []string{"/f/*"}}, false},
		{Access{Mode: "secret"}, false},
		{Access{}, false},
	} {
		if err := tc.access.Validate(); (err == nil) != tc.ok {
			t.Errorf("%+v: Validate() = %v, want ok=%v", tc.access, err, tc.ok)
		}
	}
}
