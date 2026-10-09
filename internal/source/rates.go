package source

import "math"

// validRate reports whether a rate is a usable price or multiplier: a finite
// number that is not negative.
func validRate(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }
