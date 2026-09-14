package tuner

import (
	"math"

	"gonum.org/v1/gonum/mat"
)

// Representation of the environment in which the system operates
type Environment struct {
	Lambda        float32 // request arrival rate (per minute)
	AvgInputToks  float32 // average number of prompt (input) tokens per request
	AvgOutputToks float32 // average number of output tokens per request
	MaxBatchSize  int     // maximum batch size
	AvgTTFT       float32 // average time to first token (TTFT) (msec)
	AvgITL        float32 // average inter token latency (msec)
}

func (e *Environment) Valid() bool {
	return e.Lambda > 0 &&
		!math.IsInf(float64(e.Lambda), 0) &&
		!math.IsNaN(float64(e.Lambda)) &&
		e.AvgInputToks > 0 &&
		e.AvgOutputToks > 0 &&
		e.MaxBatchSize > 0 &&
		e.AvgTTFT > 0 &&
		e.AvgITL > 0
}

func (e *Environment) GetObservations() *mat.VecDense {
	return mat.NewVecDense(2, []float64{float64(e.AvgTTFT), float64(e.AvgITL)})
}
