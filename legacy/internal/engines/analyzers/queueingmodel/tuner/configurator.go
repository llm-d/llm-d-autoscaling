package tuner

import (
	"fmt"
	"math"

	"gonum.org/v1/gonum/mat"
)

// Configurator for the model tuner
type Configurator struct {
	// dimensions
	numStates       int // number of state parameters
	numObservations int // number of observation metrics

	// matrices
	state                      *mat.VecDense // (initial or prior) values of state parameters
	stateCovariance            *mat.Dense    // covariance matrix of estimation error
	stateNoiseCovariance       *mat.Dense    // covariance matrix of noise on state
	observationNoiseCovariance *mat.Dense    // covariance matrix of noise on observation

	// functions
	stateTransitionFunc func(*mat.VecDense) *mat.VecDense // transition function for the state params

	// other
	percentStateChange []float64 // expected percent change in state params
	stateBounded       bool      // if state bounded
	minState           []float64 // min values of state params
	maxState           []float64 // max values of state params
}

func NewConfigurator(configData *TunerConfigData) (c *Configurator, err error) {
	if !checkConfigData(configData) {
		return nil, fmt.Errorf("invalid config data: %v", configData)
	}

	modelData := configData.ModelData
	numStates := len(modelData.InitState)
	X := mat.NewVecDense(numStates, modelData.InitState)

	filterData := configData.FilterData
	numObservations := len(modelData.ExpectedObservations)
	obsCOV := make([]float64, numObservations)
	factor := ((filterData.ErrorLevel / filterData.TPercentile) * (filterData.ErrorLevel / filterData.TPercentile)) / filterData.GammaFactor
	for j := range numObservations {
		obsCOV[j] = factor * modelData.ExpectedObservations[j] * modelData.ExpectedObservations[j]
	}
	R := mat.DenseCopyOf(mat.NewDiagDense(numObservations, obsCOV))

	c = &Configurator{
		numStates:                  numStates,
		numObservations:            numObservations,
		state:                      X,
		stateCovariance:            nil,
		stateNoiseCovariance:       nil,
		observationNoiseCovariance: R,
		stateTransitionFunc:        nil,
		percentStateChange:         modelData.PercentChange,
		stateBounded:               modelData.BoundedState,
		minState:                   modelData.MinState,
		maxState:                   modelData.MaxState,
	}

	// Initialize P: use provided covariance if available, otherwise compute from state
	if modelData.InitCovarianceMatrix != nil {
		c.stateCovariance = mat.NewDense(numStates, numStates, modelData.InitCovarianceMatrix)
	} else {
		c.stateCovariance, err = c.GetStateCov(X)
		if err != nil {
			return nil, err
		}
	}

	if c.stateNoiseCovariance, err = c.GetStateCov(X); err != nil {
		return nil, err
	}
	c.stateTransitionFunc = stateTransitionFunc
	return c, nil
}

func (c *Configurator) GetStateCov(x *mat.VecDense) (*mat.Dense, error) {
	if x.Len() != c.numStates {
		return nil, mat.ErrNormOrder
	}
	changeCov := make([]float64, c.numStates)
	for i := 0; i < c.numStates; i++ {
		changeCov[i] = math.Pow(c.percentStateChange[i]*x.AtVec(i), 2)
	}
	return mat.DenseCopyOf(mat.NewDiagDense(c.numStates, changeCov)), nil
}

func (c *Configurator) NumStates() int {
	return c.numStates
}

func (c *Configurator) NumObservations() int {
	return c.numObservations
}

// check validity of configuration data
func checkConfigData(cd *TunerConfigData) bool {
	if cd == nil {
		return false
	}

	// Validate FilterData
	fd := cd.FilterData
	if fd.GammaFactor <= 0 || fd.ErrorLevel <= 0 || fd.TPercentile <= 0 {
		return false
	}

	// Validate ModelData
	md := cd.ModelData

	// Check State length and values
	n := len(md.InitState)
	if n == 0 {
		return false
	}
	for _, val := range md.InitState {
		if math.IsNaN(val) || math.IsInf(val, 0) {
			return false
		}
	}

	// Check CovarianceMatrix
	if md.InitCovarianceMatrix != nil {
		if len(md.InitCovarianceMatrix) != n*n {
			return false
		}
		// check symmetry
		covMatrix := mat.NewDense(n, n, md.InitCovarianceMatrix)
		if !IsSymmetric(covMatrix, DefaultEpsilon) {
			return false
		}
	}

	// Check PercentChange length and values
	if len(md.PercentChange) != n {
		return false
	}
	// TODO: Is a zero change value acceptable?
	for _, pc := range md.PercentChange {
		if pc <= 0 || math.IsNaN(pc) || math.IsInf(pc, 0) {
			return false
		}
	}

	// Check bounded state constraints
	if md.BoundedState {
		if len(md.MinState) != n || len(md.MaxState) != n {
			return false
		}
		// Validate MinState < MaxState for each element
		for i := range n {
			if md.MinState[i] >= md.MaxState[i] {
				return false
			}
			if math.IsNaN(md.MinState[i]) || math.IsInf(md.MinState[i], 0) {
				return false
			}
			if math.IsNaN(md.MaxState[i]) || math.IsInf(md.MaxState[i], 0) {
				return false
			}
		}
	}

	// Check ExpectedObservations
	if len(md.ExpectedObservations) == 0 {
		return false
	}
	for _, obs := range md.ExpectedObservations {
		// observed metrics could be any finite numeric value (negative, zero, or positive)
		if math.IsNaN(obs) || math.IsInf(obs, 0) {
			return false
		}
	}

	return true
}

func stateTransitionFunc(x *mat.VecDense) *mat.VecDense {
	return x // identity function, no controlled dynamics
}
