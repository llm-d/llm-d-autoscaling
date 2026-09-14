package config

import (
	"math"
)

// Parameters

// Tolerated percentile for SLOs
var SLOPercentile = 0.95

// Multiplier of average of exponential distribution to attain percentile
var SLOMargin = -float32(math.Log(1 - SLOPercentile))

// maximum number of requests in queueing system as multiples of maximum batch size
var MaxQueueToBatchRatio = 10

// accelerator transition penalty factor
var AccelPenaltyFactor = float32(0.1)

// default name of a service class
const DefaultServiceClassName string = "Free"

// default priority of a lowest service class
const DefaultLowPriority int = 100

// default priority of a highest service class
const DefaultHighPriority int = 1

// default priority of a service class (lowest)
const DefaultServiceClassPriority int = DefaultLowPriority

// default option for allocation under saturated condition
var DefaultSaturatedAllocationPolicy SaturatedAllocationPolicy = None
