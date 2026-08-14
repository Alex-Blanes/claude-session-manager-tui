package main

import "testing"

func TestTruncBlockKeepsRunesIntact(t *testing.T) {
	// "máquina" is 7 runes but 8 bytes: a byte-wise cut here produces mojibake.
	if got, want := truncBlock("máquina", 7), "máquina"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	got := truncBlock("cambio de máquina", 12)
	if want := "cambio de..."; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	for _, r := range got {
		if r == '�' {
			t.Fatalf("cut landed inside a character: %q", got)
		}
	}
	// Line breaks survive, unlike trunc.
	if got := truncBlock("uno\ndos", 99); got != "uno\ndos" {
		t.Errorf("got %q, want the newline kept", got)
	}
	if got := trunc("uno\ndos", 99); got != "uno" {
		t.Errorf("trunc should stop at the first line break, got %q", got)
	}
	// Degenerate budgets must not panic or slice out of range.
	for _, n := range []int{-1, 0, 1, 3} {
		_ = truncBlock("máquina", n)
	}
}

func TestSplitBudgetGivesSlackToTheOther(t *testing.T) {
	cases := []struct{ a, b, total, wantA, wantB int }{
		{100, 100, 900, 100, 100}, // both fit: no truncation at all
		{20, 2000, 900, 20, 880},  // short first hands its slack to last
		{2000, 20, 900, 880, 20},  // and the other way round
		{2000, 2000, 900, 450, 450},
		{0, 5000, 900, 0, 900},
	}
	for _, c := range cases {
		gotA, gotB := splitBudget(c.a, c.b, c.total)
		if gotA != c.wantA || gotB != c.wantB {
			t.Errorf("splitBudget(%d,%d,%d) = %d,%d want %d,%d",
				c.a, c.b, c.total, gotA, gotB, c.wantA, c.wantB)
		}
		if gotA+gotB > c.total {
			t.Errorf("budget overrun: %d+%d > %d", gotA, gotB, c.total)
		}
	}
}
