package flaresolverr

import (
	"strings"
	"testing"
	"time"
)

func TestResolveTimeoutErrorNamesTheDeadlineThatFired(t *testing.T) {
	// The wire-compatible wording, kept byte for byte: clients match on it.
	const budgetMessage = "Error solving the challenge. Timeout after 220 seconds."

	tests := []struct {
		name         string
		maxTimeoutMS int
		elapsed      time.Duration
		want         string
		wantContains []string
	}{
		{
			name:         "budget really ran out",
			maxTimeoutMS: 220000,
			elapsed:      220 * time.Second,
			want:         budgetMessage,
		},
		{
			name:         "overshot the budget",
			maxTimeoutMS: 220000,
			elapsed:      231 * time.Second,
			want:         budgetMessage,
		},
		{
			name:         "stopped just short, inside the slack",
			maxTimeoutMS: 220000,
			elapsed:      219 * time.Second,
			want:         budgetMessage,
		},
		{
			// The measured case: a 30s WebDriver client timeout surfacing as a
			// bare DeadlineExceeded, which used to be reported as 220 seconds.
			name:         "driver gave up long before the budget",
			maxTimeoutMS: 220000,
			elapsed:      37400 * time.Millisecond,
			wantContains: []string{"37.4 seconds", "220-second maxTimeout", "stopped responding"},
		},
		{
			name:         "default budget, driver timeout",
			maxTimeoutMS: 60000,
			elapsed:      31 * time.Second,
			wantContains: []string{"31.0 seconds", "60-second maxTimeout"},
		},
		{
			name:         "default budget genuinely exhausted",
			maxTimeoutMS: 60000,
			elapsed:      60 * time.Second,
			want:         "Error solving the challenge. Timeout after 60 seconds.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveTimeoutError(tc.maxTimeoutMS, tc.elapsed).Error()

			if tc.want != "" && got != tc.want {
				t.Fatalf("message = %q, want %q", got, tc.want)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("message = %q, want it to mention %q", got, want)
				}
			}
			// Whatever else changes, the prefix decides the Prometheus label.
			if !strings.HasPrefix(got, "Error") {
				t.Errorf("message = %q, want an \"Error\" prefix so prometheusResult still counts it", got)
			}
			if got := prometheusResult(got); got != "error" {
				t.Errorf("prometheusResult = %q, want \"error\"", got)
			}
		})
	}
}

func TestResolveTimeoutErrorNeverQuotesABudgetThatWasNotReached(t *testing.T) {
	// The whole point of the fix: a short failure must not name the long budget,
	// because that sends the reader off to raise a number nothing waited on.
	err := resolveTimeoutError(220000, 37*time.Second).Error()
	if strings.Contains(err, "Timeout after 220 seconds") {
		t.Fatalf("message still blames the untouched budget: %q", err)
	}
}

func TestFormatElapsedSeconds(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{0, "0.0"},
		{-time.Second, "0.0"},
		{500 * time.Millisecond, "0.5"},
		{37400 * time.Millisecond, "37.4"},
		{220 * time.Second, "220.0"},
	}
	for _, tc := range tests {
		if got := formatElapsedSeconds(tc.in); got != tc.want {
			t.Errorf("formatElapsedSeconds(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
