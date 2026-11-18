package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerifyAccount(t *testing.T) {
	cases := []struct {
		name         string
		listStatus   int
		want         string
		wantErr      bool
		eventsCalled bool
	}{
		{name: "calendarList works", listStatus: 200, want: "user@example.com"},
		{name: "fallback to events summary", listStatus: 403, want: "summary@example.com", eventsCalled: true},
		{name: "other error", listStatus: 500, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eventsCalled := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/users/me/calendarList/primary":
					w.WriteHeader(tc.listStatus)
					if tc.listStatus == 200 {
						_, _ = w.Write([]byte(`{"id":"user@example.com"}`))
					} else {
						_, _ = fmt.Fprintf(w, `{"error":{"code":%d,"message":"fake"}}`, tc.listStatus)
					}
				case "/calendars/primary/events":
					eventsCalled = true
					_, _ = w.Write([]byte(`{"summary":"summary@example.com","items":[]}`))
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			got, err := verifyAccount(context.Background(), srv.Client(), srv.URL+"/")
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want || eventsCalled != tc.eventsCalled {
				t.Errorf("got %q (events called %v), want %q (%v)", got, eventsCalled, tc.want, tc.eventsCalled)
			}
		})
	}
}
