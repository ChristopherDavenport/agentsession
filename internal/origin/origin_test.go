package origin

import (
	"slices"
	"testing"
)

// TestPickAsksUntilAdmitted: Pick asks the read check of the sessions in
// the store's holder order and of none after the first it admits.
func TestPickAsksUntilAdmitted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		c     Candidates
		admit string
		want  string
		asked []string
	}{
		{"an owner", Candidates{Owners: []string{"a", "b", "c"}, OnPrefix: []string{"p"}}, "b", "b", []string{"a", "b"}},
		{"the chain's end", Candidates{Chain: []string{"f", "o"}, Ends: true, Owners: []string{"o", "z"}}, "fo", "o", []string{"f", "o"}},
		{"past a chain the check stops", Candidates{Chain: []string{"f", "o"}, Ends: true, Owners: []string{"o", "z"}, OnPrefix: []string{"f"}}, "f", "f", []string{"f", "o", "z"}},
		{"none", Candidates{Owners: []string{"a"}}, "", "", []string{"a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked []string
			got := tc.c.Pick(func(id string) bool {
				asked = append(asked, id)
				return id != "" && len(id) == 1 && slices.Contains([]byte(tc.admit), id[0])
			})
			if got != tc.want || !slices.Equal(asked, tc.asked) {
				t.Errorf("Pick = %q asking %v; want %q asking %v", got, asked, tc.want, tc.asked)
			}
		})
	}
}
