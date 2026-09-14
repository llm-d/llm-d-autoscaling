package tuner

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/queueing/config"
	"gonum.org/v1/gonum/mat"

	analyzer "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/queueing/analyzer"
	kalman "github.com/llm-inferno/kalman-filter/pkg/core"
)

type Tuner struct {
	configurator *Configurator
	filter       *kalman.ExtendedKalmanFilter
	env          *Environment
}

// TunedResults holds the results of parameter tuning
type TunedResults struct {
	ServiceParms     *analyzer.ServiceParms
	Innovation       *mat.VecDense
	Covariance       *mat.Dense
	NIS              float64
	ValidationFailed bool // Indicates if NIS validation failed (but previous state is returned)
}

func NewTuner(configData *TunerConfigData, env *Environment) (tuner *Tuner, err error) {
	var c *Configurator
	var f *kalman.ExtendedKalmanFilter

	// Validate inputs
	if env == nil {
		return nil, errors.New("environment cannot be nil")
	}
	if !env.Valid() {
		return nil, fmt.Errorf("invalid environment: %v", env)
	}

	// create configurator
	if c, err = NewConfigurator(configData); err != nil {
		return nil, fmt.Errorf("error on configurator creation: %v", err)
	}

	// create filter
	f, err = kalman.NewExtendedKalmanFilter(c.NumStates(), c.NumObservations(), c.state, c.stateCovariance)
	if err != nil {
		return nil, fmt.Errorf("error on filter creation: %v", err)
	}
	if err := f.SetQ(c.stateNoiseCovariance); err != nil {
		return nil, fmt.Errorf("error on setting Q: %v", err)
	}
	if err := f.SetR(c.observationNoiseCovariance); err != nil {
		return nil, fmt.Errorf("error on setting R: %v", err)
	}
	if err := f.SetfF(c.stateTransitionFunc); err != nil {
		return nil, fmt.Errorf("error on setting fFunc: %v", err)
	}
	if c.stateBounded {
		if err := f.SetStateLimiter(c.minState, c.maxState); err != nil {
			return nil, fmt.Errorf("error on setting state limiter: %v", err)
		}
	}

	// create tuner
	t := &Tuner{
		env:          env,
		configurator: c,
		filter:       f,
	}

	// assign observation function to filter
	if err := f.SethH(t.makeObservationFunc()); err != nil {
		return nil, fmt.Errorf("error on setting observation function: %v", err)
	}

	return t, nil
}

func (t *Tuner) Run() (tunedResults *TunedResults, err error) {
	// validate environment before running
	if !t.env.Valid() {
		return nil, fmt.Errorf("cannot run tuner with invalid environment: %v", t.env)
	}

	// create a stasher and stash the current X and P
	stasher, err := NewStasher(t.filter)
	if err != nil {
		return nil, fmt.Errorf("failed to create stasher: %w", err)
	}
	if err := stasher.Stash(); err != nil {
		return nil, fmt.Errorf("failed to stash filter state: %w", err)
	}

	// prediction
	Q := t.filter.Q
	if err := t.filter.Predict(Q); err != nil {
		return nil, fmt.Errorf("failed to predict: %w", err)
	}

	// update
	Z := t.env.GetObservations()
	if err := t.filter.Update(Z, t.configurator.observationNoiseCovariance); err != nil {
		return nil, fmt.Errorf("failed to update filter: %w", err)
	}

	// check validity of tunedResults
	nis, valErr := t.validateTunedResults()
	if valErr != nil {
		// unstash to return to previous filter state
		if err := stasher.UnStash(); err != nil {
			return nil, fmt.Errorf("failed to unstash filter state after validation failure: %w", err)
		}
		// Extract OLD state after unstashing
		tunedResults, extractErr := t.extractTunedResults()
		if extractErr != nil {
			return nil, fmt.Errorf("validation failed and extraction of previous state failed: %w", extractErr)
		}

		// Validate that we actually got a previous state
		if tunedResults == nil || tunedResults.ServiceParms == nil {
			return nil, fmt.Errorf("validation failed (NIS=%.6f) and no previous state available: %v", nis, valErr)
		}

		// Mark validation as failed but return previous valid state
		tunedResults.ValidationFailed = true
		// Only use the measured NIS if it's non-negative, otherwise keep -1 as diagnostic
		tunedResults.NIS = nis

		// Log both the validation error and the fact we're returning the previous state
		return tunedResults, nil // Return nil error and previous state with ValidationFailed set to true
	}

	// Extract tuned parameters
	tunedResults, err = t.extractTunedResults()
	if err != nil {
		return nil, fmt.Errorf("failed to extract tuned params: %w", err)
	}
	tunedResults.NIS = nis
	tunedResults.ValidationFailed = false
	return tunedResults, nil
}

func (t *Tuner) X() *mat.VecDense {
	return t.filter.State()
}

func (t *Tuner) Y() *mat.VecDense {
	return t.filter.Innovation()
}

func (t *Tuner) P() *mat.Dense {
	return t.filter.P
}

func (t *Tuner) S() *mat.Dense {
	return t.filter.S
}

func (t *Tuner) String() string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "Tuner: \n")
	fmt.Fprintf(&b, "%v\n", t.configurator)
	fmt.Fprintf(&b, "%v\n", t.env)
	return b.String()
}

func (t *Tuner) UpdateEnvironment(env *Environment) error {
	if env == nil {
		return errors.New("environment cannot be nil")
	}
	if !env.Valid() {
		return fmt.Errorf("invalid environment: %v", env)
	}
	t.env = env
	return nil
}

func (t *Tuner) GetParms() *mat.VecDense {
	// TODO: intelligent state return
	return t.X()
}

func (t *Tuner) GetEnvironment() *Environment {
	return t.env
}

func (t *Tuner) makeObservationFunc() func(x *mat.VecDense) *mat.VecDense {
	return func(x *mat.VecDense) *mat.VecDense {
		N := t.env.MaxBatchSize
		maxQueue := N * config.MaxQueueToBatchRatio
		qConfig := &analyzer.Configuration{
			MaxBatchSize: N,
			MaxQueueSize: maxQueue,
			ServiceParms: &analyzer.ServiceParms{
				Alpha: float32(x.AtVec(StateIndexAlpha)),
				Beta:  float32(x.AtVec(StateIndexBeta)),
				Gamma: float32(x.AtVec(StateIndexGamma)),
			},
		}
		requestData := &analyzer.RequestSize{
			AvgInputTokens:  t.env.AvgInputToks,
			AvgOutputTokens: t.env.AvgOutputToks,
		}

		qa, err := analyzer.NewQueueAnalyzer(qConfig, requestData)
		if err != nil {
			fmt.Printf("%v: model tuner observation function: failed to create queue analyzer\n", err)
			return nil
		}

		lambda := t.env.Lambda / 60 // convert to req per sec
		metrics, err := qa.Analyze(lambda)
		if err != nil {
			fmt.Printf("%v: model tuner observation function: failed to analyze queueing model\n", err)
			return nil
		}

		ttft := float64(metrics.AvgWaitTime + metrics.AvgPrefillTime)
		itl := float64(metrics.AvgTokenTime)

		return mat.NewVecDense(2, []float64{ttft, itl})
	}
}

func (t *Tuner) extractTunedResults() (*TunedResults, error) {
	stateVec := mat.VecDenseCopyOf(t.X())
	if stateVec == nil {
		return nil, errors.New("tuner returned nil state vector")
	}
	innovation := mat.VecDenseCopyOf(t.Y())
	covariance := mat.DenseCopyOf(t.P())

	return &TunedResults{
		ServiceParms: &analyzer.ServiceParms{
			Alpha: float32(stateVec.AtVec(StateIndexAlpha)),
			Beta:  float32(stateVec.AtVec(StateIndexBeta)),
			Gamma: float32(stateVec.AtVec(StateIndexGamma)),
		},
		Innovation: innovation,
		Covariance: covariance,
	}, nil
}

func (t *Tuner) validateTunedResults() (float64, error) {
	stateVec := mat.VecDenseCopyOf(t.X())
	if stateVec == nil {
		return -1.0, errors.New("tuner returned nil state vector")
	}

	// 1. check parms are positive
	if stateVec.AtVec(StateIndexAlpha) <= 0 || stateVec.AtVec(StateIndexBeta) <= 0 {
		return -1.0, fmt.Errorf("decode parameters must be positive: alpha=%f, beta=%f", stateVec.AtVec(StateIndexAlpha), stateVec.AtVec(StateIndexBeta))
	}
	if stateVec.AtVec(StateIndexGamma) <= 0 {
		return -1.0, fmt.Errorf("prefill parameters must be positive: gamma=%f", stateVec.AtVec(StateIndexGamma))
	}

	// 2. innovation check using Normalized Innovation Squared (NIS)
	innovation := mat.VecDenseCopyOf(t.Y()) // y vector
	innovationCov := mat.DenseCopyOf(t.S()) // S matrix

	// Calculate NIS = y^T * S^-1 * y
	S_inv := mat.NewDense(innovationCov.RawMatrix().Rows, innovationCov.RawMatrix().Cols, nil)
	if err := S_inv.Inverse(innovationCov); err != nil {
		return -1.0, fmt.Errorf("singular innovation covariance matrix S encountered: %w", err)
	}

	// tmp = S^-1 * y
	tmp := mat.NewVecDense(S_inv.RawMatrix().Rows, nil)
	tmp.MulVec(S_inv, innovation)

	// NIS = y^T * tmp
	NIS := mat.Dot(innovation, tmp)

	if NIS >= DefaultMaxNIS {
		// Return the actual computed NIS along with an error so callers can record the measured NIS value and failure in validation.
		return NIS, fmt.Errorf("normalized innovation squared (NIS=%.2f) exceeds threshold (%.2f), rejecting update as outlier",
			NIS, DefaultMaxNIS)
	}

	// 3. estimate covariance check?
	// TODO

	return NIS, nil
}
