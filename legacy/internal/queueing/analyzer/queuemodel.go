package analyzer

import (
	"bytes"
	"fmt"
)

// Basic Queueing Model (Abstract Class)
type QueueModel struct {
	lambda float32 // arrival rate
	mu     float32 // service rate
	rho    float32 // utilization (average number of customers in service)

	avgRespTime    float32 // average response time (waiting + service)
	avgWaitTime    float32 // average waiting time
	avgServTime    float32 // average service time
	avgNumInSystem float32 // average total number of customers in system (waiting + in service)
	avgQueueLength float32 // average queue length
	isValid        bool    // validity of input data

	ComputeRho        func() float32 // compute utilization of queueing model
	GetRhoMax         func() float32 // compute the maximum utilization of queueing model
	computeStatistics func()         // evaluate performance measures of queueing model
}

// Solve queueing model given arrival and service rates
func (m *QueueModel) Solve(lambda float32, mu float32) {
	m.lambda = lambda
	m.mu = mu
	m.rho = m.ComputeRho()
	if (m.rho < 0) || (m.rho >= m.GetRhoMax()) || (lambda < 0) || (mu <= 0) {
		m.isValid = false
	} else {
		m.isValid = true
		m.computeStatistics()
	}
}

func (m *QueueModel) IsValid() bool {
	return m.isValid
}

func (m *QueueModel) GetLambda() float32 {
	return m.lambda
}

func (m *QueueModel) GetMu() float32 {
	return m.mu
}

func (m *QueueModel) GetRho() float32 {
	return m.rho
}

func (m *QueueModel) GetAvgQueueLength() float32 {
	return m.avgQueueLength
}

func (m *QueueModel) GetAvgNumInSystem() float32 {
	return m.avgNumInSystem
}

func (m *QueueModel) GetAvgWaitTime() float32 {
	return m.avgWaitTime
}

func (m *QueueModel) GetAvgServTime() float32 {
	return m.avgServTime
}

func (m *QueueModel) GetAvgRespTime() float32 {
	return m.avgRespTime
}

func (m *QueueModel) String() string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "isValid=%v; ", m.isValid)
	fmt.Fprintf(&b, "lambda=%v; mu=%v; rho=%v; ", m.lambda, m.mu, m.rho)
	if m.isValid {
		fmt.Fprintf(&b, "T=%v; W=%v; X=%v; ", m.avgRespTime, m.avgWaitTime, m.avgServTime)
		fmt.Fprintf(&b, "N=%v; Q=%v; ", m.avgNumInSystem, m.avgQueueLength)
	}
	return b.String()
}
