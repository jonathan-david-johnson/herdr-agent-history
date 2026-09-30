package history

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

func assertState(t testing.TB, got State, entries []string, cursor int) {
	t.Helper()
	if !slices.Equal(got.Entries, entries) || got.Cursor != cursor {
		t.Fatalf("state = %#v; want entries %q, cursor %d", got, entries, cursor)
	}
	assertInvariant(t, got)
}

func assertInvariant(t testing.TB, s State) {
	t.Helper()
	if len(s.Entries) == 0 {
		if s.Cursor != -1 || s.Current() != "" {
			t.Fatalf("invalid empty state: %#v", s)
		}
		return
	}
	if s.Cursor < 0 || s.Cursor >= len(s.Entries) {
		t.Fatalf("invalid cursor: %#v", s)
	}
	seen := make(map[string]bool)
	for _, id := range s.Entries {
		if blank(id) || seen[id] {
			t.Fatalf("blank or duplicate ID %q in %#v", id, s)
		}
		seen[id] = true
	}
	if s.Current() != s.Entries[s.Cursor] {
		t.Fatalf("Current disagrees with cursor: %#v", s)
	}
}

func TestNewAndJSON(t *testing.T) {
	s := New()
	assertState(t, s, nil, -1)
	s.Visit("terminal:42")
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"entries":["terminal:42"],"cursor":0}` {
		t.Fatalf("unexpected JSON: %s", data)
	}
	var loaded State
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatal(err)
	}
	assertState(t, loaded, []string{"terminal:42"}, 0)
}

func TestNormalizationAndReadOnlyMethods(t *testing.T) {
	tests := []struct {
		name    string
		input   State
		entries []string
		cursor  int
	}{
		{"zero value", State{}, nil, -1},
		{"empty high cursor", State{Cursor: 9}, nil, -1},
		{"empty negative cursor", State{Cursor: -9}, nil, -1},
		{"negative clamps first", State{[]string{"a", "b"}, -5}, []string{"a", "b"}, 0},
		{"high clamps last", State{[]string{"a", "b"}, 5}, []string{"a", "b"}, 1},
		{"duplicate selected", State{[]string{"a", "b", "a", "c"}, 2}, []string{"a", "b", "c"}, 0},
		{"duplicate before selected", State{[]string{"a", "a", "b"}, 2}, []string{"a", "b"}, 1},
		{"blank selected predecessor", State{[]string{"a", "", "b"}, 1}, []string{"a", "b"}, 0},
		{"blank selected first survivor", State{[]string{"", " ", "b"}, 0}, []string{"b"}, 0},
		{"all blank", State{[]string{"", " \t\n"}, 1}, nil, -1},
		{"opaque IDs preserved", State{[]string{" a ", "a", "term:/Ω"}, 0}, []string{" a ", "a", "term:/Ω"}, 0},
		{"combined invalid high", State{[]string{"", "a", "a", " ", "b", ""}, 99}, []string{"a", "b"}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := State{Entries: slices.Clone(tt.input.Entries), Cursor: tt.input.Cursor}
			current := ""
			if tt.cursor >= 0 {
				current = tt.entries[tt.cursor]
			}
			if got := tt.input.Current(); got != current {
				t.Fatalf("Current = %q; want %q", got, current)
			}
			clone := tt.input.Clone()
			assertState(t, clone, tt.entries, tt.cursor)
			if !reflect.DeepEqual(tt.input, original) {
				t.Fatalf("read-only methods changed input: %#v -> %#v", original, tt.input)
			}
			assertState(t, clone.Clone(), tt.entries, tt.cursor)
			if len(clone.Entries) > 0 {
				clone.Entries[0] = "changed"
				if !reflect.DeepEqual(tt.input, original) {
					t.Fatal("clone shares its backing array with input")
				}
			}
		})
	}
}

func TestVisit(t *testing.T) {
	tests := []struct {
		name    string
		input   State
		id      string
		entries []string
		cursor  int
	}{
		{"empty", New(), "a", []string{"a"}, 0},
		{"empty blank", New(), "", nil, -1},
		{"empty whitespace", New(), " \t", nil, -1},
		{"append", State{[]string{"a", "b"}, 1}, "c", []string{"a", "b", "c"}, 2},
		{"preserve forward tail", State{[]string{"mcps", "herdr"}, 0}, "ubuntu", []string{"mcps", "ubuntu", "herdr"}, 1},
		{"current unchanged", State{[]string{"a", "b", "c"}, 1}, "b", []string{"a", "b", "c"}, 1},
		{"revisit before cursor", State{[]string{"a", "b", "c", "d"}, 2}, "a", []string{"b", "c", "a", "d"}, 2},
		{"revisit immediate predecessor", State{[]string{"a", "b", "c"}, 1}, "a", []string{"b", "a", "c"}, 1},
		{"revisit after cursor", State{[]string{"a", "b", "c", "d"}, 1}, "d", []string{"a", "b", "d", "c"}, 2},
		{"revisit immediate successor", State{[]string{"a", "b", "c"}, 0}, "b", []string{"a", "b", "c"}, 1},
		{"blank normalizes loaded state", State{[]string{"", "a", "a", "b"}, 2}, "", []string{"a", "b"}, 0},
		{"negative cursor", State{[]string{"a", "b"}, -4}, "c", []string{"a", "c", "b"}, 1},
		{"high cursor", State{[]string{"a", "b"}, 8}, "c", []string{"a", "b", "c"}, 2},
		{"opaque ID", New(), " a ", []string{" a "}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.input
			s.Visit(tt.id)
			assertState(t, s, tt.entries, tt.cursor)
			s.Visit(tt.id)
			assertState(t, s, tt.entries, tt.cursor)
		})
	}
}

func TestTailPreservingBranch(t *testing.T) {
	s := New()
	s.Visit("mcps")
	s.Visit("herdr")
	if got := s.Previous(); got != (Decision{"mcps", "history_previous"}) {
		t.Fatalf("Previous = %#v", got)
	}
	s.Visit("ubuntu")
	assertState(t, s, []string{"mcps", "ubuntu", "herdr"}, 1)
	if got := s.Next([]string{"done"}, "ubuntu", []string{"mcps", "ubuntu", "done", "herdr"}); got != (Decision{"herdr", "history_next"}) {
		t.Fatalf("Next = %#v", got)
	}
	assertState(t, s, []string{"mcps", "ubuntu", "herdr"}, 2)
}

func TestPrune(t *testing.T) {
	tests := []struct {
		name    string
		input   State
		live    map[string]bool
		entries []string
		cursor  int
	}{
		{"empty", New(), map[string]bool{"a": true}, nil, -1},
		{"nil removes all", State{[]string{"a", "b"}, 1}, nil, nil, -1},
		{"false removes", State{[]string{"a", "b"}, 1}, map[string]bool{"a": true, "b": false}, []string{"a"}, 0},
		{"preserve selected identity", State{[]string{"a", "b", "c", "d"}, 2}, map[string]bool{"b": true, "c": true, "d": true}, []string{"b", "c", "d"}, 1},
		{"selected removed closest preceding", State{[]string{"a", "b", "c", "d"}, 2}, map[string]bool{"a": true, "b": true, "d": true}, []string{"a", "b", "d"}, 1},
		{"selected removed sparse preceding", State{[]string{"a", "b", "c", "d"}, 2}, map[string]bool{"a": true, "d": true}, []string{"a", "d"}, 0},
		{"selected removed no preceding", State{[]string{"a", "b", "c", "d"}, 1}, map[string]bool{"c": true, "d": true}, []string{"c", "d"}, 0},
		{"first removed", State{[]string{"a", "b", "c"}, 0}, map[string]bool{"b": true, "c": true}, []string{"b", "c"}, 0},
		{"last removed", State{[]string{"a", "b", "c"}, 2}, map[string]bool{"a": true, "b": true}, []string{"a", "b"}, 1},
		{"normalize first", State{[]string{"a", "b", "a", ""}, 2}, map[string]bool{"a": true, "b": true}, []string{"a", "b"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.input
			s.Prune(tt.live)
			assertState(t, s, tt.entries, tt.cursor)
			s.Prune(tt.live)
			assertState(t, s, tt.entries, tt.cursor)
		})
	}
}

func TestPrevious(t *testing.T) {
	tests := []struct {
		name   string
		input  State
		cursor int
		want   Decision
	}{
		{"empty", New(), -1, Decision{"", "no_destination"}},
		{"singleton", State{[]string{"a"}, 0}, 0, Decision{"a", "unchanged"}},
		{"backward", State{[]string{"a", "b", "c"}, 2}, 1, Decision{"b", "history_previous"}},
		{"wrap", State{[]string{"a", "b", "c"}, 0}, 2, Decision{"c", "history_wrap_previous"}},
		{"negative cursor wraps", State{[]string{"a", "b"}, -10}, 1, Decision{"b", "history_wrap_previous"}},
		{"high cursor clamps", State{[]string{"a", "b", "c"}, 10}, 1, Decision{"b", "history_previous"}},
		{"duplicates normalize", State{[]string{"a", "a", "b"}, 1}, 1, Decision{"b", "history_wrap_previous"}},
		{"all blanks", State{[]string{"", " "}, 0}, -1, Decision{"", "no_destination"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.input
			entries := s.Clone().Entries
			if got := s.Previous(); got != tt.want {
				t.Fatalf("Previous = %#v; want %#v", got, tt.want)
			}
			assertState(t, s, entries, tt.cursor)
		})
	}
}

func TestNext(t *testing.T) {
	tests := []struct {
		name    string
		input   State
		done    []string
		current string
		all     []string
		entries []string
		cursor  int
		want    Decision
	}{
		{"empty", New(), nil, "", nil, nil, -1, Decision{"", "no_destination"}},
		{"empty completion", New(), []string{"b"}, "a", []string{"a", "b"}, []string{"b"}, 0, Decision{"b", "completion"}},
		{"empty current skipped", New(), []string{"a"}, "a", []string{"a"}, nil, -1, Decision{"", "no_destination"}},
		{"singleton unchanged", State{[]string{"a"}, 0}, nil, "a", []string{"a"}, []string{"a"}, 0, Decision{"a", "unchanged"}},
		{"singleton current done skipped", State{[]string{"a"}, 0}, []string{"a"}, "a", []string{"a"}, []string{"a"}, 0, Decision{"a", "unchanged"}},
		{"singleton completion", State{[]string{"a"}, 0}, []string{"b"}, "a", []string{"a", "b"}, []string{"a", "b"}, 1, Decision{"b", "completion"}},
		{"forward beats completion", State{[]string{"a", "b", "c"}, 0}, []string{"d"}, "a", []string{"a", "d", "b", "c"}, []string{"a", "b", "c"}, 1, Decision{"b", "history_next"}},
		{"forward without completion", State{[]string{"a", "b", "c"}, 1}, nil, "b", nil, []string{"a", "b", "c"}, 2, Decision{"c", "history_next"}},
		{"wrap", State{[]string{"a", "b", "c"}, 2}, nil, "c", []string{"a", "b", "c"}, []string{"a", "b", "c"}, 0, Decision{"a", "history_wrap_next"}},
		{"only current done wraps", State{[]string{"a", "b"}, 1}, []string{"b"}, "b", []string{"a", "b"}, []string{"a", "b"}, 0, Decision{"a", "history_wrap_next"}},
		{"completion append", State{[]string{"a", "b"}, 1}, []string{"c"}, "b", []string{"a", "b", "c"}, []string{"a", "b", "c"}, 2, Decision{"c", "completion"}},
		{"done move to end", State{[]string{"a", "b", "c"}, 2}, []string{"a"}, "c", []string{"a", "b", "c"}, []string{"b", "c", "a"}, 2, Decision{"a", "completion"}},
		{"middle done move to end", State{[]string{"a", "b", "c"}, 2}, []string{"b"}, "c", []string{"a", "b", "c"}, []string{"a", "c", "b"}, 2, Decision{"b", "completion"}},
		{"cyclic scan beats done order", State{[]string{"b"}, 0}, []string{"a", "d", "c"}, "b", []string{"a", "b", "c", "d"}, []string{"b", "c"}, 1, Decision{"c", "completion"}},
		{"cyclic scan wraps sidebar", State{[]string{"d"}, 0}, []string{"c", "a"}, "d", []string{"a", "b", "c", "d"}, []string{"d", "a"}, 1, Decision{"a", "completion"}},
		{"skip current choose next", State{[]string{"b"}, 0}, []string{"b", "c"}, "b", []string{"a", "b", "c"}, []string{"b", "c"}, 1, Decision{"c", "completion"}},
		{"skip disallowed sidebar entries", State{[]string{"a"}, 0}, []string{"d"}, "a", []string{"a", "b", "c", "d"}, []string{"a", "d"}, 1, Decision{"d", "completion"}},
		{"missing current starts first", New(), []string{"b", "a"}, "gone", []string{"a", "b"}, []string{"a"}, 0, Decision{"a", "completion"}},
		{"done absent from sidebar", New(), []string{"gone"}, "a", []string{"a", "b"}, nil, -1, Decision{"", "no_destination"}},
		{"empty sidebar no candidate", New(), []string{"a"}, "", nil, nil, -1, Decision{"", "no_destination"}},
		{"blank and duplicate done IDs", New(), []string{"", " ", "b", "b"}, "a", []string{"", "a", " ", "b", "b"}, []string{"b"}, 0, Decision{"b", "completion"}},
		{"negative cursor normalizes before forward", State{[]string{"a", "b"}, -5}, []string{"c"}, "a", []string{"a", "c", "b"}, []string{"a", "b"}, 1, Decision{"b", "history_next"}},
		{"high cursor normalizes before completion", State{[]string{"a", "b"}, 50}, []string{"c"}, "b", []string{"a", "b", "c"}, []string{"a", "b", "c"}, 2, Decision{"c", "completion"}},
		{"duplicates normalize before forward", State{[]string{"a", "a", "b", ""}, 1}, []string{"c"}, "a", []string{"a", "c", "b"}, []string{"a", "b"}, 1, Decision{"b", "history_next"}},
		{"opaque IDs", New(), []string{" b "}, "a", []string{"a", "b", " b "}, []string{" b "}, 0, Decision{" b ", "completion"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.input
			done, all := slices.Clone(tt.done), slices.Clone(tt.all)
			if got := s.Next(tt.done, tt.current, tt.all); got != tt.want {
				t.Fatalf("Next = %#v; want %#v", got, tt.want)
			}
			assertState(t, s, tt.entries, tt.cursor)
			if !slices.Equal(done, tt.done) || !slices.Equal(all, tt.all) {
				t.Fatal("Next changed sidebar input slices")
			}
		})
	}
}

func TestPlansDoNotMutateOriginal(t *testing.T) {
	original := State{[]string{"a", "b", "c"}, 1}
	plan := original.Clone()
	plan.Previous()
	plan.Visit("d")
	plan.Prune(map[string]bool{"d": true, "c": true})
	plan.Next([]string{"e"}, "d", []string{"d", "e", "c"})
	assertState(t, original, []string{"a", "b", "c"}, 1)
	original = plan.Clone()
	plan.Visit("f")
	assertState(t, original, []string{"d", "c"}, 1)
}

func FuzzReducerInvariants(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 255})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte("mcps herdr ubuntu"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 512 {
			data = data[:512]
		}
		ids := []string{"", " ", "mcps", "herdr", "ubuntu", " opaque ", "term:/Ω"}
		s := New()
		for i, b := range data {
			if i == 8 {
				break
			}
			s.Entries = append(s.Entries, ids[int(b)%len(ids)])
		}
		if len(data) > 0 {
			s.Cursor = int(data[0]) - 128
		}
		s = s.Clone()
		assertInvariant(t, s)
		for _, b := range data {
			before := s.Clone()
			other := s.Clone()
			id := ids[int(b)%len(ids)]
			var got, want Decision
			switch b % 4 {
			case 0:
				s.Visit(id)
				other.Visit(id)
				if !blank(id) {
					if s.Current() != id {
						t.Fatal("Visit did not select visited ID")
					}
					without := func(entries []string) []string {
						result := make([]string, 0, len(entries))
						for _, entry := range entries {
							if entry != id {
								result = append(result, entry)
							}
						}
						return result
					}
					if !slices.Equal(without(before.Entries), without(s.Entries)) {
						t.Fatal("Visit lost or reordered other history entries")
					}
					s.Visit(id)
				}
			case 1:
				live := make(map[string]bool)
				for i, candidate := range ids {
					live[candidate] = b&(1<<i) != 0
				}
				s.Prune(live)
				other.Prune(live)
				if live[before.Current()] && s.Current() != before.Current() {
					t.Fatal("Prune lost a surviving selected identity")
				}
				s.Prune(live)
			case 2:
				got, want = s.Previous(), other.Previous()
				if !slices.Equal(s.Entries, before.Entries) {
					t.Fatal("Previous changed history entries")
				}
			case 3:
				done := []string{id, ids[(int(b)+1)%len(ids)]}
				got = s.Next(done, before.Current(), ids)
				want = other.Next(done, before.Current(), ids)
				if before.Cursor >= 0 && before.Cursor+1 < len(before.Entries) {
					if got != (Decision{before.Entries[before.Cursor+1], "history_next"}) || !slices.Equal(s.Entries, before.Entries) {
						t.Fatal("Next did not prioritize the forward history tail")
					}
				}
				if got.Reason == "completion" && got.Target == before.Current() {
					t.Fatal("Next selected the current sidebar ID as a completion")
				}
			}
			assertInvariant(t, s)
			assertState(t, other, s.Entries, s.Cursor)
			if got != want {
				t.Fatalf("non-deterministic decision: %#v != %#v", got, want)
			}
			if got.Target != "" && got.Target != s.Current() {
				t.Fatal("planned decision disagrees with state selection")
			}
		}
	})
}
