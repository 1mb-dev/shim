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
