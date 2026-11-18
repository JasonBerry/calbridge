package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	tests := []struct {
		args     []string
		code     int
		stdout   string
		stderrIn string
	}{
		{args: []string{"version"}, code: 0, stdout: "calbridge dev\n"},
		{args: nil, code: 2, stderrIn: "usage: calbridge"},
		{args: []string{"help"}, code: 0, stderrIn: "usage: calbridge"},
		{args: []string{"nope"}, code: 2, stderrIn: `unknown command "nope"`},
		{args: []string{"serve"}, code: 1, stderrIn: "not implemented"},
	}
	for _, tc := range tests {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var out, errb bytes.Buffer
			code := run(context.Background(), tc.args, &out, &errb)
			if code != tc.code {
				t.Fatalf("exit code = %d, want %d (stderr %q)", code, tc.code, errb.String())
			}
			if tc.stdout != "" && out.String() != tc.stdout {
				t.Errorf("stdout = %q, want %q", out.String(), tc.stdout)
			}
			if !strings.Contains(errb.String(), tc.stderrIn) {
				t.Errorf("stderr %q does not contain %q", errb.String(), tc.stderrIn)
			}
		})
	}
}

func TestUsageHidesSpike(t *testing.T) {
	var b bytes.Buffer
	usage(&b)
	if strings.Contains(b.String(), "\n  spike ") {
		t.Errorf("usage lists hidden command: %s", b.String())
	}
}
