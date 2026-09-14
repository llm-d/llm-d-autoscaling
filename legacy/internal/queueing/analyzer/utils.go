package analyzer

import (
	"fmt"
	"math"
)

var epsilon float32 = 1e-6
var maxIterations int = 100

// A variable x is relatively within a given tolerance from a value
func WithinTolerance(x, value, tolerance float32) bool {
	if x == value {
		return true
	}
	if value == 0 || tolerance < 0 {
		return false
	}
	return math.Abs(float64((x-value)/value)) <= float64(tolerance)
}

// Binary search: find xStar in a range [xMin, xMax] such that f(xStar)=yTarget.
// Function f() must be monotonically increasing or decreasing over the range.
// Returns an indicator of whether target is below (-1), within (0), or above (+1) the bounded region.
// Returns an error if the function cannot be evaluated or the target is not found.
func BinarySearch(xMin float32, xMax float32, yTarget float32,
	eval func(float32) (float32, error)) (float32, int, error) {

	if xMin > xMax {
		return 0, 0, fmt.Errorf("invalid range [%v, %v]", xMin, xMax)
	}

	// evaluate the function at the boundaries
	yBounds := make([]float32, 2)
	var err error
	for i, x := range []float32{xMin, xMax} {
		if yBounds[i], err = eval(x); err != nil {
			return 0, 0, fmt.Errorf("invalid function evaluation: %v", err)
		}
		if WithinTolerance(yBounds[i], yTarget, epsilon) {
			return x, 0, nil
		}
	}

	increasing := yBounds[0] < yBounds[1]
	if increasing && yTarget < yBounds[0] || !increasing && yTarget > yBounds[0] {
		return xMin, -1, nil // target is below the bounded region
	}
	if increasing && yTarget > yBounds[1] || !increasing && yTarget < yBounds[1] {
		return xMax, +1, nil // target is above the bounded region
	}

	// perform binary search
	var xStar, yStar float32
	for range maxIterations {
		xStar = 0.5 * (xMin + xMax)
		if yStar, err = eval(xStar); err != nil {
			return 0, 0, fmt.Errorf("invalid function evaluation: %v", err)
		}
		if WithinTolerance(yStar, yTarget, epsilon) {
			break
		}
		if increasing && yTarget < yStar || !increasing && yTarget > yStar {
			xMax = xStar
		} else {
			xMin = xStar
		}
	}
	return xStar, 0, nil
}

// Function used in binary search (target service time)
func EvalServTime(model *MM1ModelStateDependent) func(x float32) (float32, error) {
	return func(x float32) (float32, error) {
		model.Solve(x, 1)
		if !model.IsValid() {
			return 0, fmt.Errorf("invalid model %v", model)
		}
		return model.GetAvgServTime(), nil
	}
}

// Function used in binary search (target waiting time)
func EvalWaitingTime(model *MM1ModelStateDependent) func(x float32) (float32, error) {
	return func(x float32) (float32, error) {
		model.Solve(x, 1)
		if !model.IsValid() {
			return 0, fmt.Errorf("invalid model %v", model)
		}
		return model.GetAvgWaitTime(), nil
	}
}

// check validity of configuration parameters
func (c *Configuration) check() error {
	if c.MaxBatchSize <= 0 || c.MaxQueueSize < 0 || c.MaxNumTokens < 0 ||
		c.ServiceParms == nil {
		return fmt.Errorf("invalid configuration %s", c)
	}
	if c.MaxNumTokens == 0 {
		c.MaxNumTokens = DefaultMaxNumTokens
	}
	return nil
}

// check validity of request size
func (rq *RequestSize) check() error {
	if rq.AvgInputTokens < 0 || rq.AvgOutputTokens < 1 {
		return fmt.Errorf("invalid request size %s", rq)
	}
	return nil
}

// check validity of target values
func (targetPerf *TargetPerf) check() error {
	if targetPerf.TargetITL < 0 ||
		targetPerf.TargetTTFT < 0 ||
		targetPerf.TargetTPS < 0 {
		return fmt.Errorf("invalid target data values %s", targetPerf)
	}
	return nil
}

/*
 * toString() functions
 */

func (c *Configuration) String() string {
	return fmt.Sprintf("{maxBatch=%d, maxNumTokens=%d, maxQueue=%d, servParms:%s}",
		c.MaxBatchSize, c.MaxNumTokens, c.MaxQueueSize, c.ServiceParms)
}

func (qa *QueueAnalyzer) String() string {
	return fmt.Sprintf("{maxBatch=%d, maxNumTokens=%d, maxQueue=%d, servParms:%s, reqSize:%s, model:%s, rates:%s}",
		qa.MaxBatchSize, qa.MaxNumTokens, qa.MaxQueueSize, qa.ServiceParms, qa.RequestSize, qa.Model, qa.RateRange)
}

func (sp *ServiceParms) String() string {
	return fmt.Sprintf("{alpha=%.3f, beta=%.5f, gamma=%.5f}", sp.Alpha, sp.Beta, sp.Gamma)
}

func (rq *RequestSize) String() string {
	return fmt.Sprintf("{inTokens=%.1f, outTokens=%.1f}", rq.AvgInputTokens, rq.AvgOutputTokens)
}

func (rr *RateRange) String() string {
	return fmt.Sprintf("[%.3f, %.3f]", rr.Min, rr.Max)
}

func (am *AnalysisMetrics) String() string {
	return fmt.Sprintf("{tput=%.3f, lat=%.3f, wait=%.3f, conc=%.3f, ttft=%.3f, itl=%.3f, maxRate=%.3f, rho=%0.3f}",
		am.Throughput, am.AvgRespTime, am.AvgWaitTime, am.AvgNumInServ, am.AvgTTFT, am.AvgTokenTime, am.MaxRate, am.Rho)
}

func (tp *TargetPerf) String() string {
	return fmt.Sprintf("{TTFT=%.3f, ITL=%.3f, TPS=%.3f}",
		tp.TargetTTFT, tp.TargetITL, tp.TargetTPS)
}

func (tr *TargetRate) String() string {
	return fmt.Sprintf("{rateTTFT=%.3f, rateITL=%.3f, rateTPS=%.3f}",
		tr.RateTargetTTFT, tr.RateTargetITL, tr.RateTargetTPS)
}
