package throughput

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ObservationWindow", func() {
	var (
		window *ObservationWindow
		now    time.Time
	)

	BeforeEach(func() {
		now = time.Now()
		window = newObservationWindow(
			DefaultWindowMaxSize,
			DefaultObservationMaxAge,
			DefaultMinSamples,
			DefaultMinKSpread,
			DefaultMinObservableK,
			DefaultMaxObservableK,
		)
	})

	Describe("Add", func() {
		It("accepts observations within [minK, maxK]", func() {
			window.Add(0.50, 0.040, now)
			Expect(window.Len()).To(Equal(1))
		})

		It("rejects k below minK (0.15)", func() {
			window.Add(0.10, 0.040, now)
			Expect(window.Len()).To(Equal(0))
		})

		It("rejects k above maxK (0.85)", func() {
			window.Add(0.90, 0.040, now)
			Expect(window.Len()).To(Equal(0))
		})

		It("rejects NaN k", func() {
			window.Add(float64NaN(), 0.040, now)
			Expect(window.Len()).To(Equal(0))
		})

		It("rejects zero ITL", func() {
			window.Add(0.50, 0.0, now)
			Expect(window.Len()).To(Equal(0))
		})

		It("rejects negative ITL", func() {
			window.Add(0.50, -0.001, now)
			Expect(window.Len()).To(Equal(0))
		})

		It("rejects NaN ITL", func() {
			window.Add(0.50, float64NaN(), now)
			Expect(window.Len()).To(Equal(0))
		})

		It("accepts k exactly at minK boundary", func() {
			window.Add(DefaultMinObservableK, 0.040, now)
			Expect(window.Len()).To(Equal(1))
		})

		It("accepts k exactly at maxK boundary", func() {
			window.Add(DefaultMaxObservableK, 0.040, now)
			Expect(window.Len()).To(Equal(1))
		})

		It("evicts the oldest observation when at capacity", func() {
			small := newObservationWindow(3, DefaultObservationMaxAge,
				DefaultMinSamples, DefaultMinKSpread, DefaultMinObservableK, DefaultMaxObservableK)

			t0 := now
			small.Add(0.20, 0.020, t0)
			small.Add(0.40, 0.035, t0.Add(time.Second))
			small.Add(0.60, 0.050, t0.Add(2*time.Second))
			Expect(small.Len()).To(Equal(3))

			// Adding a 4th evicts the first (k=0.20)
			small.Add(0.75, 0.060, t0.Add(3*time.Second))
			Expect(small.Len()).To(Equal(3))

			obs := small.Observations()
			Expect(obs[0].K).To(Equal(0.40)) // oldest remaining
		})
	})

	Describe("Prune", func() {
		It("removes observations older than maxAge", func() {
			old := now.Add(-35 * time.Minute)
			window.Add(0.40, 0.035, old)
			window.Add(0.60, 0.050, now)

			window.Prune(now)
			Expect(window.Len()).To(Equal(1))
			Expect(window.Observations()[0].K).To(Equal(0.60))
		})

		It("keeps observations younger than maxAge", func() {
			window.Add(0.40, 0.035, now.Add(-10*time.Minute))
			window.Add(0.60, 0.050, now)

			window.Prune(now)
			Expect(window.Len()).To(Equal(2))
		})

		It("removes all observations if all are stale", func() {
			old := now.Add(-60 * time.Minute)
			window.Add(0.40, 0.035, old)
			window.Add(0.60, 0.050, old)

			window.Prune(now)
			Expect(window.Len()).To(Equal(0))
		})
	})

	Describe("KSpread", func() {
		It("returns 0 when window is empty", func() {
			Expect(window.KSpread()).To(Equal(0.0))
		})

		It("returns 0 when all observations have the same k", func() {
			window.Add(0.50, 0.040, now)
			window.Add(0.50, 0.042, now)
			Expect(window.KSpread()).To(Equal(0.0))
		})

		It("returns max_k - min_k", func() {
			window.Add(0.20, 0.020, now)
			window.Add(0.50, 0.040, now)
			window.Add(0.75, 0.060, now)
			Expect(window.KSpread()).To(BeNumerically("~", 0.55, 0.001))
		})
	})

	Describe("Ready", func() {
		It("returns false when window is empty", func() {
			Expect(window.Ready()).To(BeFalse())
		})

		It("returns false when sample count is below minimum", func() {
			// Add 9 samples with wide spread — not enough samples
			for i := 0; i < 9; i++ {
				k := 0.20 + float64(i)*0.07
				window.Add(k, 0.030+k*0.04, now)
			}
			Expect(window.Len()).To(BeNumerically("<", DefaultMinSamples))
			Expect(window.Ready()).To(BeFalse())
		})

		It("returns false when spread is below minimum (all observations clustered)", func() {
			for i := 0; i < DefaultMinSamples; i++ {
				// All k values close together: 0.40 to 0.45
				k := 0.40 + float64(i)*0.005
				window.Add(k, 0.035+k*0.01, now)
			}
			Expect(window.KSpread()).To(BeNumerically("<", DefaultMinKSpread))
			Expect(window.Ready()).To(BeFalse())
		})

		It("returns true when both sample count and spread requirements are met", func() {
			// 10 samples spanning k = 0.20 to 0.70 (spread = 0.50 > 0.30)
			for i := 0; i < DefaultMinSamples; i++ {
				k := 0.20 + float64(i)*0.05
				window.Add(k, 0.020+k*0.05, now)
			}
			Expect(window.Len()).To(BeNumerically(">=", DefaultMinSamples))
			Expect(window.KSpread()).To(BeNumerically(">=", DefaultMinKSpread))
			Expect(window.Ready()).To(BeTrue())
		})
	})

	Describe("Clear", func() {
		It("removes all observations", func() {
			window.Add(0.40, 0.035, now)
			window.Add(0.60, 0.050, now)
			window.Clear()
			Expect(window.Len()).To(Equal(0))
		})

		It("resets KSpread to 0", func() {
			window.Add(0.20, 0.020, now)
			window.Add(0.80, 0.070, now)
			window.Clear()
			Expect(window.KSpread()).To(Equal(0.0))
		})

		It("allows new observations to be added after Clear", func() {
			window.Add(0.40, 0.035, now)
			window.Clear()
			window.Add(0.55, 0.045, now)
			Expect(window.Len()).To(Equal(1))
		})
	})

	Describe("Observations", func() {
		It("returns a copy that does not affect the window", func() {
			window.Add(0.40, 0.035, now)
			obs := window.Observations()
			obs[0].K = 999.0 // mutate the copy

			Expect(window.Observations()[0].K).To(Equal(0.40)) // original unchanged
		})
	})
})
