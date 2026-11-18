package spike

import (
	"testing"

	calendar "google.golang.org/api/calendar/v3"
)

func TestCheckOrganizer(t *testing.T) {
	ev := &calendar.Event{Organizer: &calendar.EventOrganizer{Email: "organizer@example.com"}}
	cases := []struct {
		send, confirm string
		ok            bool
	}{
		{"none", "", true},
		{"all", "", false},
		{"all", "someone@example.com", false},
		{"all", "Organizer@Example.com", true},
		{"externalOnly", "", false},
	}
	for _, tc := range cases {
		err := checkOrganizer(ev, tc.send, tc.confirm)
		if (err == nil) != tc.ok {
			t.Errorf("send=%s confirm=%q: err = %v", tc.send, tc.confirm, err)
		}
	}
}
