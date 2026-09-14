package analyzer

import (
	"bytes"
	"fmt"
	"math"
)

// M/M/1/K Finite storage single server queue
type MM1KModel struct {
	QueueModel           // extends base class
	K          int       // limit on number in system
	p          []float64 // state probabilities
	sumP       float64   // sum of probabilities
	throughput float32   // effective (departure) rate
}

func NewMM1KModel(k int) *MM1KModel {
	m := &MM1KModel{
		QueueModel: QueueModel{},
		K:          k,
		p:          make([]float64, k+1),
		throughput: 0,
	}
	m.QueueModel.GetRhoMax = m.GetRhoMax
	m.QueueModel.ComputeRho = m.ComputeRho
	m.QueueModel.computeStatistics = m.computeStatistics
	return m
}

// Solve queueing model given arrival and service rates
func (m *MM1KModel) Solve(lambda float32, mu float32) {
	m.QueueModel.Solve(lambda, mu)
}

// Compute utilization of queueing model
func (m *MM1KModel) ComputeRho() float32 {
	if m.lambda == m.mu {
		return 1
	} else {
		return m.lambda / m.mu
	}
}

// Compute the maximum utilization of queueing model
func (m *MM1KModel) GetRhoMax() float32 {
	return float32(m.K)
}

// Compute state probabilities
func (m *MM1KModel) computeProbabilities() {
	for i := 0; i <= m.K; i++ {
		m.p[i] = 0
	}
	m.sumP = 1
	if !m.isValid {
		m.p[0] = 1
	}
	// Compute p[0]
	if m.rho == 1 {
		m.p[0] = 1 / float64(m.K+1)
	} else {
		m.p[0] = (1 - float64(m.rho)) / (1 - math.Pow(float64(m.rho), float64(m.K+1)))
	}
	// Compute p[i], i=1,2, ..., K
	m.sumP = 0
	for i := 0; i <= m.K; i++ {
		m.p[i] = m.p[0] * math.Pow(float64(m.rho), float64(i))
		m.sumP += m.p[i]
	}
}

// Evaluate performance measures of queueing model
func (m *MM1KModel) computeStatistics() {
	if !m.isValid {
		return
	}
	m.computeProbabilities()
	var temp float64
	for i := 0; i <= m.K; i++ {
		temp += float64(i) * m.p[i]
	}
	m.avgNumInSystem = float32(temp)
	m.throughput = m.lambda * (1 - float32(m.p[m.K]))
	m.avgRespTime = m.avgNumInSystem / m.throughput
	m.avgServTime = 1 / m.mu
	m.avgWaitTime = m.avgRespTime - m.avgServTime
	if m.avgWaitTime < 0 {
		m.avgWaitTime = 0
	}
	m.avgQueueLength = m.throughput * m.avgWaitTime
}

func (m *MM1KModel) GetProbabilities() []float64 {
	return m.p
}

func (m *MM1KModel) GetThroughput() float32 {
	return m.throughput
}

func (m *MM1KModel) String() string {
	var b bytes.Buffer
	b.WriteString("MM1KModel: ")
	b.WriteString(m.QueueModel.String())
	fmt.Fprintf(&b, "tput=%v; K=%d; sumP=%v; ", m.throughput, m.K, m.sumP)
	return b.String()
}
