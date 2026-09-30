// Package history plans deterministic navigation between opaque terminal IDs.
// Navigation methods mutate their receiver; callers can plan on Clone and commit
// the resulting state only after a successful focus operation.
package history

import "strings"

// State is a unique ordered history and the index of its selected entry.
// An empty history has Cursor -1.
type State struct {
	Entries []string `json:"entries"`
	Cursor  int      `json:"cursor"`
}

// Decision describes a planned focus target and why it was selected.
type Decision struct {
	Target string
	Reason string
}

// New returns an empty history.
func New() State { return State{Cursor: -1} }

// Current returns the selected ID, or an empty string for an empty history.
// Malformed loaded state is interpreted without changing the receiver.
func (s State) Current() string {
	s.normalize()
	if s.Cursor < 0 {
		return ""
	}
	return s.Entries[s.Cursor]
}

// Clone returns an independent, normalized copy of the history.
func (s State) Clone() State {
	s.normalize()
	return s
}

// Visit records external focus immediately after the current entry. Revisited
// IDs are moved rather than duplicated, and the remaining forward tail survives.
// Empty and whitespace-only IDs are ignored; other IDs are preserved verbatim.
func (s *State) Visit(id string) {
	s.normalize()
	if blank(id) || (s.Cursor >= 0 && s.Entries[s.Cursor] == id) {
		return
	}
	if len(s.Entries) == 0 {
		s.Entries = append(s.Entries, id)
		s.Cursor = 0
		return
	}

	for i, entry := range s.Entries {
		if entry != id {
			continue
		}
		s.Entries = append(s.Entries[:i], s.Entries[i+1:]...)
		if i < s.Cursor {
			s.Cursor--
		}
		break
	}
	insert := s.Cursor + 1
	s.Entries = append(s.Entries, "")
	copy(s.Entries[insert+1:], s.Entries[insert:])
	s.Entries[insert] = id
	s.Cursor = insert
}

// Prune removes IDs that are not marked true in live. It preserves the selected
// identity when possible; otherwise it selects the closest surviving predecessor
// or, when there is no predecessor, the first surviving entry.
func (s *State) Prune(live map[string]bool) {
	s.normalize()
	entries := make([]string, 0, len(s.Entries))
	cursor := -1
	for i, id := range s.Entries {
		if !live[id] {
			continue
		}
		entries = append(entries, id)
		if i <= s.Cursor {
			cursor = len(entries) - 1
		}
	}
	if len(entries) > 0 && cursor < 0 {
		cursor = 0
	}
	s.Entries, s.Cursor = entries, cursor
}

// Previous selects the preceding history entry, wrapping at the beginning.
func (s *State) Previous() Decision {
	s.normalize()
	if len(s.Entries) == 0 {
		return Decision{Reason: "no_destination"}
	}
	if len(s.Entries) == 1 {
		return Decision{Target: s.Entries[0], Reason: "unchanged"}
	}
	if s.Cursor == 0 {
		s.Cursor = len(s.Entries) - 1
		return Decision{Target: s.Entries[s.Cursor], Reason: "history_wrap_previous"}
	}
	s.Cursor--
	return Decision{Target: s.Entries[s.Cursor], Reason: "history_previous"}
}

// Next first traverses the forward history tail. At its end, it scans the full
// sidebar cyclically after currentInSidebarOrder for an allowed done ID, skipping
// the current sidebar ID. If the current sidebar ID is absent, scanning starts at
// the first sidebar entry. IDs absent from allInSidebarOrder are not candidates.
// A completion is moved to the history end; without one, navigation wraps.
func (s *State) Next(doneInSidebarOrder []string, currentInSidebarOrder string, allInSidebarOrder []string) Decision {
	s.normalize()
	if s.Cursor >= 0 && s.Cursor+1 < len(s.Entries) {
		s.Cursor++
		return Decision{Target: s.Entries[s.Cursor], Reason: "history_next"}
	}

	if target := nextDone(doneInSidebarOrder, currentInSidebarOrder, allInSidebarOrder); target != "" {
		for i, id := range s.Entries {
			if id == target {
				s.Entries = append(s.Entries[:i], s.Entries[i+1:]...)
				break
			}
		}
		s.Entries = append(s.Entries, target)
		s.Cursor = len(s.Entries) - 1
		return Decision{Target: target, Reason: "completion"}
	}
	if len(s.Entries) == 0 {
		return Decision{Reason: "no_destination"}
	}
	s.Cursor = 0
	if len(s.Entries) == 1 {
		return Decision{Target: s.Entries[0], Reason: "unchanged"}
	}
	return Decision{Target: s.Entries[0], Reason: "history_wrap_next"}
}

func nextDone(done []string, current string, all []string) string {
	allowed := make(map[string]bool, len(done))
	for _, id := range done {
		if !blank(id) {
			allowed[id] = true
		}
	}
	start := 0
	for i, id := range all {
		if id == current {
			start = i + 1
			break
		}
	}
	for offset := 0; offset < len(all); offset++ {
		id := all[(start+offset)%len(all)]
		if id != current && allowed[id] {
			return id
		}
	}
	return ""
}

func blank(id string) bool { return strings.TrimSpace(id) == "" }

// normalize copies the backing array, drops blank IDs and later duplicates, and
// preserves the selected identity. Invalid cursors clamp to the nearest endpoint
// before cleanup. A blank selection falls back to its nearest valid predecessor,
// or the first entry. Copying also keeps Current and Clone from mutating aliases.
func (s *State) normalize() {
	cursor := s.Cursor
	if cursor < 0 {
		cursor = 0
	}
	if cursor >= len(s.Entries) {
		cursor = len(s.Entries) - 1
	}
	selected := ""
	if cursor >= 0 {
		selected = s.Entries[cursor]
	}
	entries := make([]string, 0, len(s.Entries))
	seen := make(map[string]int, len(s.Entries))
	fallback := -1
	for i, id := range s.Entries {
		if blank(id) {
			continue
		}
		index, exists := seen[id]
		if !exists {
			index = len(entries)
			seen[id] = index
			entries = append(entries, id)
		}
		if i <= cursor {
			fallback = index
		}
	}
	if index, ok := seen[selected]; ok {
		cursor = index
	} else if fallback >= 0 {
		cursor = fallback
	} else if len(entries) > 0 {
		cursor = 0
	} else {
		cursor = -1
	}
	s.Entries, s.Cursor = entries, cursor
}
