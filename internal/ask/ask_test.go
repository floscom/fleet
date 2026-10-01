package ask

import (
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	qs := []Question{
		{Question: "Color?", Options: []Option{{Label: "Red"}, {Label: "Blue"}}},
		{Question: "Fruits?", MultiSelect: true, Options: []Option{{Label: "Apple"}, {Label: "Pear"}}},
	}
	type A = map[string][]string
	long := strings.Repeat("x", maxText+1)
	tests := []struct {
		name string
		a    Answer
		ok   bool
	}{
		{"all answered", Answer{Answers: A{"Color?": {"Red"}, "Fruits?": {"Apple", "Pear"}}}, true},
		{"typed answers", Answer{Answers: A{"Color?": {"teal"}, "Fruits?": {"Apple", "Pear", "kiwi"}}}, true},
		{"with a note", Answer{Answers: A{"Color?": {"Red"}, "Fruits?": {"Pear"}}, Notes: map[string]string{"Color?": "dark"}}, true},
		{"declined", Answer{Decline: "why do you ask?"}, true},
		{"one missing", Answer{Answers: A{"Color?": {"Red"}}}, false},
		{"unknown question", Answer{Answers: A{"Color?": {"Red"}, "Fruits?": {"Pear"}, "Size?": {"S"}}}, false},
		{"two for a single select", Answer{Answers: A{"Color?": {"Red", "Blue"}, "Fruits?": {"Pear"}}}, false},
		{"too many", Answer{Answers: A{"Color?": {"Red"}, "Fruits?": {"a", "b", "c", "d"}}}, false},
		{"empty answer", Answer{Answers: A{"Color?": {""}, "Fruits?": {"Pear"}}}, false},
		{"no answer", Answer{Answers: A{"Color?": {}, "Fruits?": {"Pear"}}}, false},
		{"too long", Answer{Answers: A{"Color?": {long}, "Fruits?": {"Pear"}}}, false},
		{"note on nothing", Answer{Answers: A{"Color?": {"Red"}, "Fruits?": {"Pear"}}, Notes: map[string]string{"Size?": "x"}}, false},
		{"declined with answers", Answer{Decline: "no", Answers: A{"Color?": {"Red"}}}, false},
		{"nothing", Answer{}, false},
	}
	for _, tt := range tests {
		if err := tt.a.Check(qs); (err == nil) != tt.ok {
			t.Errorf("%s: %v", tt.name, err)
		}
	}
}
