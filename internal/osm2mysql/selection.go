package osm2mysql

import (
	"fmt"
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

type tagSelection struct {
	key, value string
}

type tagSelections []tagSelection

func (s tagSelection) matchesValue(value string) bool {
	if s.value == "*" {
		return value != ""
	}
	return s.value == value
}

func (s tagSelections) forEachMatch(tags map[string]string, visit func(int, tagSelection)) {
	for i, pair := range s {
		value, ok := tags[pair.key]
		if !ok || !pair.matchesValue(value) {
			continue
		}
		duplicate := false
		for _, previous := range s[:i] {
			if previous.key == pair.key && previous.matchesValue(value) {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		if pair.value == "*" {
			value = truncateUTF8(strings.ToValidUTF8(value, ""), 200)
		}
		visit(i, tagSelection{pair.key, value})
	}
}

func (s *tagSelections) String() string {
	var values []string
	for _, pair := range *s {
		values = append(values, pair.key+"="+pair.value)
	}
	return strings.Join(values, ", ")
}

func (s *tagSelections) Set(input string) error {
	key, value, ok := strings.Cut(input, "=")
	if !ok || key == "" || value == "" {
		return fmt.Errorf("tag must be a non-empty name=value pair")
	}
	if !utf8.ValidString(key) || !utf8.ValidString(value) {
		return fmt.Errorf("tag name and value must be valid UTF-8")
	}
	key = strings.ToLower(key)
	if utf8.RuneCountInString(key) > 50 || utf8.RuneCountInString(value) > 200 {
		return fmt.Errorf("tag name and value must be at most 50 and 200 characters")
	}
	pair := tagSelection{key, value}
	for _, existing := range *s {
		if existing == pair {
			return nil
		}
	}
	*s = append(*s, pair)
	return nil
}

func (s tagSelections) matches(tags map[string]string) bool {
	for _, pair := range s {
		if value, ok := tags[pair.key]; ok && pair.matchesValue(value) {
			return true
		}
	}
	return false
}

type selectionMatches struct {
	selections tagSelections
	matched    []atomic.Bool
}

func newSelectionMatches(selections tagSelections) *selectionMatches {
	return &selectionMatches{selections: selections, matched: make([]atomic.Bool, len(selections))}
}

func (m *selectionMatches) record(tags map[string]string) {
	for i, pair := range m.selections {
		if !m.matched[i].Load() {
			if value, ok := tags[pair.key]; ok && pair.matchesValue(value) {
				m.matched[i].Store(true)
			}
		}
	}
}

func (m *selectionMatches) logUnmatched(log *Logger) {
	for i, pair := range m.selections {
		if !m.matched[i].Load() {
			log.Log(fmt.Sprintf("ERROR: tag pair %q=%q matched no nodes or ways; check spelling and input coverage", pair.key, pair.value))
		}
	}
}
