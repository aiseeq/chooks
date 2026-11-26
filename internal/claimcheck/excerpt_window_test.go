package claimcheck

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestExcerptSelectionExactLength(t *testing.T) {
	// Exercise gaps at both edges, split and merged gaps, and digit boundaries.
	for _, n := range []int{1, 9, 10, 11, 99, 100, 101} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			lines := make([]string, n)
			lengths := make([]int, n)
			selected := make([]bool, n)
			for i := range lines {
				lines[i] = strings.Repeat("я", i%7)
				lengths[i] = utf8.RuneCountInString(lines[i])
			}
			state := excerptSelection{selected: selected, lengths: lengths, tree: make([]int, n+1), omitted: 1, markerRunes: excerptMarkerLength(n)}
			check := func() {
				t.Helper()
				actual := utf8.RuneCountInString((excerptRenderer{lines: lines, lengths: lengths, selected: selected}).render())
				if !state.fits(actual) || state.fits(actual-1) {
					t.Fatalf("rendered %d, accounting: %+v", actual, state)
				}
			}
			check()
			for i := 0; i < n; i += 2 {
				state.set(i, true)
				check()
			}
			for i := 1; i < n; i += 2 {
				state.set(i, true)
				check()
			}
			for i := n - 1; i >= 0; i-- {
				state.set(i, false)
				check()
			}
		})
	}
}

func TestExcerptWindowsReordersAfterSelection(t *testing.T) {
	// Selecting the first window makes the overlapping one cheaper than the
	// previously higher-ranked third window. Only one more window fits.
	lines := []string{"a", "first", "12345678901234567890", "third", "b", "cc", "abcdefghijklmnopqrst", "dd"}
	lengths := make([]int, len(lines))
	for i := range lines {
		lengths[i] = len(lines[i])
	}
	words := [][]string{nil, {"alpha", "beta", "gamma"}, nil, {"delta"}, nil, nil, {"epsilon"}, nil}
	remaining := excerptRemainingWords(words)
	selected := make([]bool, len(lines))
	windows := excerptWindows(lines, words, lengths, remaining)
	fitExcerptWindows(windows, lines, words, lengths, remaining, selected, 50)
	if !selected[3] || selected[6] {
		t.Fatalf("greedy choice = %v, want overlapping window before third", selected)
	}
}

// Compare with a deliberately straightforward greedy implementation that
// renders every trial. Small seeded cases cover overlapping and repeated words.
func TestExcerptWindowsMatchGreedyReference(t *testing.T) {
	rng := rand.New(rand.NewSource(4711))
	for trial := range 300 {
		n := 2 + rng.Intn(24)
		lines := make([]string, n)
		lengths := make([]int, n)
		words := make([][]string, n)
		for i := range lines {
			lines[i] = strings.Repeat("x", 1+rng.Intn(30))
			lengths[i] = len(lines[i])
			for range 3 {
				if rng.Intn(5) == 0 {
					words[i] = append(words[i], fmt.Sprintf("w%d", rng.Intn(8)))
				}
			}
		}
		limit := 15 + rng.Intn(150)
		want := referenceExcerptWindows(lines, words, lengths, limit)
		got := make([]bool, n)
		remaining := excerptRemainingWords(words)
		fitExcerptWindows(excerptWindows(lines, words, lengths, remaining), lines, words, lengths, remaining, got, limit)
		if !slices.Equal(got, want) {
			t.Fatalf("trial %d limit %d: got %v, want %v; words %v, lengths %v", trial, limit, got, want, words, lengths)
		}
	}
}

func referenceExcerptWindows(lines []string, words [][]string, lengths []int, limit int) []bool {
	selected := make([]bool, len(lines))
	attempted := make([]bool, len(lines))
	remaining := excerptRemainingWords(words)
	renderer := excerptRenderer{lines: lines, lengths: lengths, selected: selected}
	for {
		best, bestScore, bestCost := -1, 0, 1
		for i := range lines {
			if attempted[i] || len(words[i]) == 0 {
				continue
			}
			score := countVisibleWords(neighboringAnswerWords(words, i), remaining)
			cost := 0
			for j := max(0, i-1); j <= min(len(lines)-1, i+1); j++ {
				if !selected[j] {
					cost += lengths[j] + 1
				}
			}
			cost = max(cost, 1)
			if score*bestCost > bestScore*cost || score*bestCost == bestScore*cost && score > bestScore {
				best, bestScore, bestCost = i, score, cost
			}
		}
		if best < 0 || bestScore == 0 {
			break
		}
		attempted[best] = true
		from, to := max(0, best-1), min(len(lines)-1, best+1)
		var added []int
		for j := from; j <= to; j++ {
			if !selected[j] {
				selected[j] = true
				added = append(added, j)
			}
		}
		if utf8.RuneCountInString(renderer.render()) > limit {
			for _, j := range added {
				selected[j] = false
			}
			if !selected[best] {
				selected[best] = true
				if utf8.RuneCountInString(renderer.render()) > limit {
					selected[best] = false
					continue
				}
			}
		}
		for j := from; j <= to; j++ {
			if selected[j] {
				for _, word := range words[j] {
					delete(remaining, word)
				}
			}
		}
	}
	return selected
}
