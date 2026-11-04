package main

import "testing"

func TestPercentile(t *testing.T) {
	limits := []int64{10, 100, 1000}
	tests := []struct {
		name   string
		counts []uint64
		share  float64
		want   string
	}{
		{"nothing arrived", []uint64{0, 0, 0, 0}, 0.5, "-"},
		{"most are quick", []uint64{90, 9, 1, 0}, 0.5, "≤10 ms"},
		{"the slow few set the p99", []uint64{90, 9, 1, 0}, 0.99, "≤100 ms"},
		{"the oldest is the last one", []uint64{90, 9, 1, 0}, 1, "≤1000 ms"},
		{"older than every limit", []uint64{1, 0, 0, 1}, 1, ">1000 ms"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := percentile(tt.counts, limits, tt.share); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
