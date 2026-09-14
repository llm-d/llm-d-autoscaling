package tuner

// Tuner configuration data
type TunerConfigData struct {
	FilterData FilterData     // filter data
	ModelData  TunerModelData // model data
}

// Filter configuration data
type FilterData struct {
	GammaFactor float64 // gamma factor
	ErrorLevel  float64 // error level percentile
	TPercentile float64 // tail of student distribution
}

// Model configuration data
type TunerModelData struct {
	InitState            []float64 // initial state of model parameters (X vector, size n) (may be overwritten)
	InitCovarianceMatrix []float64 // (flat) initial covariance matrix (P matrix, size nxn) (may be overwritten), could be nil
	PercentChange        []float64 // percent change in state (size n)
	BoundedState         bool      // are the state values bounded in a range
	MinState             []float64 // lower bound on state (size n)
	MaxState             []float64 // upper bound on state (size n)
	ExpectedObservations []float64 // expected values of observations (size m)
}
