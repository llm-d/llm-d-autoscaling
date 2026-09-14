package saturation_v2

import "time"

// rollingAverage maintains a fixed-size sliding window of float64 values
// and computes their arithmetic mean. Used to smooth noisy compute-capacity
// (k2) observations over time.
type rollingAverage struct {
	values      []float64
	maxSize     int
	lastUpdated time.Time
}

// newRollingAverage creates a rollingAverage with the given window size.
func newRollingAverage(maxSize int) *rollingAverage {
	return &rollingAverage{
		values:      make([]float64, 0, maxSize),
		maxSize:     maxSize,
		lastUpdated: time.Now(),
	}
}

// Add appends a value, evicting the oldest entry if the window is full.
func (r *rollingAverage) Add(value float64) {
	if len(r.values) >= r.maxSize {
		r.values = r.values[1:]
	}
	r.values = append(r.values, value)
	r.lastUpdated = time.Now()
}

// Average returns the arithmetic mean of all stored values, or 0 if empty.
func (r *rollingAverage) Average() float64 {
	if len(r.values) == 0 {
		return 0
	}
	var sum float64
	for _, v := range r.values {
		sum += v
	}
	return sum / float64(len(r.values))
}

// Len returns the number of values currently stored.
func (r *rollingAverage) Len() int {
	return len(r.values)
}
