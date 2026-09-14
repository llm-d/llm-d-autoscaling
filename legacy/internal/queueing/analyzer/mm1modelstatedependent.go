package analyzer

import (
	"bytes"
	"math"
)

// M/M/1 model with state dependent service rate
type MM1ModelStateDependent struct {
	MM1KModel                 // extends base class
	servRate        []float32 // state-dependent service rate
	avgNumInServers float32
}

func NewMM1ModelStateDependent(k int, servRate []float32) *MM1ModelStateDependent {
	m := MM1ModelStateDependent{
		MM1KModel:       *NewMM1KModel(k),
		servRate:        servRate,
		avgNumInServers: 0,
	}

	m.QueueModel.ComputeRho = m.ComputeRho
	m.QueueModel.computeStatistics = m.computeStatistics
	return &m
}

// Solve queueing model given arrival and service rates
func (m *MM1ModelStateDependent) Solve(lambda float32, mu float32) {
	m.MM1KModel.Solve(lambda, mu)
}

// Compute utilization of queueing model
func (m *MM1ModelStateDependent) ComputeRho() float32 {
	return 1 - float32(m.p[0])
}

// Evaluate performance measures of queueing model
func (m *MM1ModelStateDependent) computeStatistics() {
	if !m.isValid {
		return
	}
	m.computeProbabilities()

	// calculate avgNumInServers
	num := len(m.servRate)
	var avgNumInServers float64
	var avgNumInSystem float64
	sumP := m.p[0]
	for i := 1; i <= m.K; i++ {
		avgNumInSystem += float64(i) * m.p[i]
		sumP += m.p[i]
		if i == num {
			avgNumInServers = avgNumInSystem + (1-sumP)*float64(num)
		}
	}
	m.avgNumInServers = float32(avgNumInServers)
	m.avgNumInSystem = float32(avgNumInSystem)

	m.throughput = m.lambda * (1 - float32(m.p[m.K]))
	m.avgRespTime = m.avgNumInSystem / m.throughput
	m.avgServTime = m.avgNumInServers / m.throughput
	m.avgWaitTime = m.avgRespTime - m.avgServTime
	if m.avgWaitTime < 0 {
		m.avgWaitTime = 0
	}
	m.avgQueueLength = m.throughput * m.avgWaitTime
}

// Compute state probabilities
func (m *MM1ModelStateDependent) computeProbabilities() {
	// queue length distribution
	// p[i] = Probability[system has exactly i customers]
	m.p[0] = 1
	scale := math.MaxFloat64 / float64(m.K)
	var sRate float64
	num := len(m.servRate)
	for n := 0; n < m.K; n++ {
		if n < num {
			sRate = float64(m.servRate[n])
		} else {
			sRate = float64(m.servRate[num-1])
		}
		m.p[n+1] = m.p[n] * float64(m.lambda) / sRate
		for m.p[n+1] < 0 || math.IsInf(m.p[n+1], 0) || math.IsNaN(m.p[n+1]) {
			for i := 0; i <= n; i++ {
				m.p[i] /= scale
			}
			m.p[n+1] = m.p[n] * float64(m.lambda) / sRate
		}
	}

	// normalize queue length distribution
	var sum float64
	for n := 0; n <= m.K; n++ {
		sum += m.p[n]
		if sum < 0 || math.IsInf(sum, 0) {
			sum = 0
			for i := 0; i <= m.K; i++ {
				m.p[i] /= scale
				if i <= n {
					sum += m.p[i]
				}
			}
		}
	}

	// queue length distribution
	m.sumP = 0
	for n := 0; n <= m.K; n++ {
		m.p[n] /= sum
		m.sumP += m.p[n]
	}

	// calculate rho
	m.rho = m.ComputeRho()
}

func (m *MM1ModelStateDependent) GetAvgNumInServers() float32 {
	return m.avgNumInServers
}

func (m *MM1ModelStateDependent) String() string {
	var b bytes.Buffer
	b.WriteString("MM1ModelStateDependent: ")
	b.WriteString(m.MM1KModel.String())
	// fmt.Fprintf(&b, "servRate=%v; ", m.servRate)
	return b.String()
}
