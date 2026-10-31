package main

import (
	"math"
	"sort"
)

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// medianMAD returns the median and a normal-consistent sigma estimate
// (1.4826 * MAD). MAD is used instead of the standard deviation so that the
// very print a detection is meant to catch cannot inflate the baseline it is
// compared against.
func medianMAD(v []float64) (float64, float64) {
	if len(v) == 0 {
		return 0, 0
	}
	m := median(v)
	dev := make([]float64, len(v))
	for i, x := range v {
		dev[i] = math.Abs(x - m)
	}
	return m, 1.4826 * median(dev)
}
