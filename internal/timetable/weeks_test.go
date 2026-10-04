package timetable

import (
	"slices"
	"testing"
)

func TestExpandWeeksAccepted(t *testing.T) {
	tests := []struct {
		name string
		spec string
		want []int
	}{
		{"single week", "1", []int{1}},
		{"single week range", "5-5", []int{5}},
		{"contiguous range", "1-16", []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}},
		{"list with range", "1,3,5-9", []int{1, 3, 5, 6, 7, 8, 9}},
		{"unsorted list is sorted", "9,1,5", []int{1, 5, 9}},
		{"duplicates collapse", "2,2,1-3", []int{1, 2, 3}},
		{"overlapping ranges collapse", "1-3,2-4", []int{1, 2, 3, 4}},
		{"surrounding whitespace", "  3 , 1-2  ", []int{1, 2, 3}},
		{"whitespace around the dash", "2 - 4", []int{2, 3, 4}},
		{"maximum week", "60", []int{60}},
		{"range to the maximum week", "58-60", []int{58, 59, 60}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExpandWeeks(tt.spec)
			if err != nil {
				t.Fatalf("ExpandWeeks(%q) error = %v, want nil", tt.spec, err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("ExpandWeeks(%q) = %v, want %v", tt.spec, got, tt.want)
			}
		})
	}
}

func TestExpandWeeksRejected(t *testing.T) {
	tests := []struct {
		name string
		spec string
	}{
		{"empty", ""},
		{"blank", "   "},
		{"leading comma", ",1"},
		{"trailing comma", "1,"},
		{"empty entry between commas", "1,,2"},
		{"zero", "0"},
		{"negative", "-2"},
		{"reversed range", "3-1"},
		{"zero lower bound", "0-3"},
		{"week above the maximum", "61"},
		{"range above the maximum", "59-61"},
		{"non numeric", "abc"},
		{"mixed numeric and text", "1,x"},
		{"dash only", "-"},
		{"missing upper bound", "1-"},
		{"missing lower bound", "-3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExpandWeeks(tt.spec)
			if err == nil {
				t.Fatalf("ExpandWeeks(%q) = %v, want an error", tt.spec, got)
			}
			if got != nil {
				t.Fatalf("ExpandWeeks(%q) returned weeks %v alongside the error %v", tt.spec, got, err)
			}
		})
	}
}

// TestExpandWeeksCoversMaxWeek pins the documented bound: a single cell cannot
// expand past week 60.
func TestExpandWeeksCoversMaxWeek(t *testing.T) {
	if MaxWeek != 60 {
		t.Fatalf("MaxWeek = %d, want 60", MaxWeek)
	}
	weeks, err := ExpandWeeks("1-60")
	if err != nil {
		t.Fatalf("ExpandWeeks(1-60) error = %v, want nil", err)
	}
	if len(weeks) != 60 || weeks[0] != 1 || weeks[59] != 60 {
		t.Fatalf("ExpandWeeks(1-60) = %v, want 60 weeks from 1 to 60", weeks)
	}
}
