package tokens

import "testing"

func TestApproximate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"single char", "a", 1},
		{"four chars one token", "abcd", 1},
		{"eight chars two tokens", "abcdefgh", 2},
		{"long english sentence", "the quick brown fox jumps over the lazy dog", 10}, // 43/4 = 10
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Approximate(tc.in); got != tc.want {
				t.Errorf("Approximate(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestApproximateMessages(t *testing.T) {
	if got := ApproximateMessages(nil); got != 0 {
		t.Errorf("nil = %d, want 0", got)
	}
	// Two 8-char messages → 2*(2+4) = 12.
	got := ApproximateMessages([]string{"abcdefgh", "abcdefgh"})
	if got != 12 {
		t.Errorf("got %d, want 12", got)
	}
}

func TestCountConcat(t *testing.T) {
	// "ab cd" = 5 chars → 5/4 = 1
	if got := CountConcat("ab", "cd"); got != 1 {
		t.Errorf("got %d, want 1", got)
	}
}
