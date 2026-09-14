package tuner

// Default filter parameters

// infinitesimal value to check float equality
const DefaultEpsilon = 1e-6

// Default tuner parameters
const (
	// scaling factor used to adjust the calculated measurement noise variance (> 0).
	DefaultGammaFactor = 1.0

	/*
		DefaultErrorLevel,  DefaultTPercentile along with DefaultGammaFactor is used in initializing the measurement noise covariance matrix R.
		Particularly, R is assumed to be diagonal, meaning measurement errors are independent across different measures ($z_i$).
		The measurement of a performance metric of interest, say z_i, is expected to have a 95% Confidence Interval (CI) of +-5% of its mean value.
		This means that the error (standard deviation) around z_i is expected to be 5% (DefaultErrorLevel).
		However, we assume that the measurement noise (error) follows a t-distribution.
		Under the asymptotic limit, the t-distribution converges to a standard normal distribution, the critical value of which is 1.96 (DefaultTPercentile).
	*/
	DefaultErrorLevel  = 0.05
	DefaultTPercentile = 1.96

	// determines the limit on the amount of change in a state value per iteration. A small number results in the filter converging relatively slowly.
	DefaultPercentChange = 0.05

	// default parameter values (state vector)
	DefaultAlpha = 5.0
	DefaultBeta  = 0.05
	DefaultGamma = 0.00005

	// default min and max state factors determine the lower and upper bound on the state values.
	DefaultMinStateFactor = 0.01
	DefaultMaxStateFactor = 100.0

	// default expected observation values
	DefaultExpectedTTFT = 50.0
	DefaultExpectedITL  = 5.0

	/*
		Under nominal conditions, the NIS (Normalized Innovations Squared) of a Kalman Filter is expected to follow
		a Chi-Squared Distribution with degrees of freedom equal to the dimension of the measurement vector (n = 2 for [ttft, itl]).
		Here, we enforce that a tuner update is accepted for 95% confidence interval of NIS.
		The upper bound of the interval in our case is 7.378.
	*/
	DefaultMaxNIS = 7.378

	// Transient delay to allow scaled up servers to start serving requests before tuning is applied
	TransientDelaySeconds = 120

	// State vector indices for model parameters
	StateIndexAlpha = 0 // Dbase parameter
	StateIndexBeta  = 1 // compute slope parameter
	StateIndexGamma = 2 // memory access slope parameter

	// Initial parameter estimation factors
	BaseFactor = 0.9 // fraction of metric value (ITL or TTFT) assumed base value (alpha or gamma) in range (0, 1)
)
