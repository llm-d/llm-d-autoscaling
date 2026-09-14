package throughput

import (
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ShapeTracker", func() {
	var tracker *ShapeTracker

	BeforeEach(func() {
		tracker = newShapeTracker(DefaultShapeChangeTolerance)
	})

	Describe("first Observe call", func() {
		It("sets the shape without reporting a change", func() {
			shape, changed := tracker.Observe(5000, 200, 0.1)

			Expect(changed).To(BeFalse())
			Expect(shape.AvgInputTokens).To(Equal(5000.0))
			Expect(shape.AvgOutputTokens).To(Equal(200.0))
			Expect(shape.PrefixHitRate).To(Equal(0.1))
		})

		It("computes ILeff and KVreq correctly", func() {
			shape, _ := tracker.Observe(5000, 200, 0.3)

			// ILeff = 5000 × (1 - 0.3) = 3500
			Expect(shape.ILeff).To(BeNumerically("~", 3500.0, 0.01))
			// KVreq = 3500 + 200/2 = 3600
			Expect(shape.KVreq).To(BeNumerically("~", 3600.0, 0.01))
		})

		It("reports no shape before first call", func() {
			_, hasShape := tracker.Current()
			Expect(hasShape).To(BeFalse())
		})
	})

	Describe("subsequent calls within tolerance", func() {
		BeforeEach(func() {
			tracker.Observe(5000, 200, 0.0)
		})

		It("does not report a change for identical values", func() {
			_, changed := tracker.Observe(5000, 200, 0.0)
			Expect(changed).To(BeFalse())
		})

		It("does not report a change for 15% IL shift (within 20% tolerance)", func() {
			_, changed := tracker.Observe(5750, 200, 0.0) // +15% IL
			Expect(changed).To(BeFalse())
		})

		It("does not report a change for 15% OL shift (within 20% tolerance)", func() {
			_, changed := tracker.Observe(5000, 230, 0.0) // +15% OL
			Expect(changed).To(BeFalse())
		})

		It("does not report a change at exactly the tolerance boundary", func() {
			_, changed := tracker.Observe(6000, 200, 0.0) // exactly +20% IL
			Expect(changed).To(BeFalse())
		})
	})

	Describe("shape change detection", func() {
		BeforeEach(func() {
			tracker.Observe(5000, 200, 0.0)
		})

		It("reports a change for >20% IL increase", func() {
			_, changed := tracker.Observe(6100, 200, 0.0) // +22% IL
			Expect(changed).To(BeTrue())
		})

		It("reports a change for >20% IL decrease", func() {
			_, changed := tracker.Observe(3900, 200, 0.0) // -22% IL
			Expect(changed).To(BeTrue())
		})

		It("reports a change for >20% OL increase", func() {
			_, changed := tracker.Observe(5000, 245, 0.0) // +22.5% OL
			Expect(changed).To(BeTrue())
		})

		It("updates the stored shape after a change", func() {
			tracker.Observe(6500, 300, 0.0)
			shape, hasShape := tracker.Current()
			Expect(hasShape).To(BeTrue())
			Expect(shape.AvgInputTokens).To(Equal(6500.0))
			Expect(shape.AvgOutputTokens).To(Equal(300.0))
		})
	})

	Describe("Reset", func() {
		It("clears the stored shape", func() {
			tracker.Observe(5000, 200, 0.0)
			tracker.Reset()

			_, hasShape := tracker.Current()
			Expect(hasShape).To(BeFalse())
		})

		It("treats the next call as a fresh first call after Reset", func() {
			tracker.Observe(5000, 200, 0.0)
			tracker.Reset()

			_, changed := tracker.Observe(9999, 999, 0.0)
			Expect(changed).To(BeFalse())
		})
	})

	Describe("NaN and edge case hit rates", func() {
		It("treats NaN hit rate as 0.0", func() {
			shape, _ := tracker.Observe(5000, 200, float64NaN())
			Expect(shape.PrefixHitRate).To(Equal(0.0))
			Expect(shape.ILeff).To(BeNumerically("~", 5000.0, 0.01))
		})

		It("clamps hit rate above 1.0 to 1.0", func() {
			shape, _ := tracker.Observe(5000, 200, 1.5)
			Expect(shape.PrefixHitRate).To(Equal(1.0))
			Expect(shape.ILeff).To(BeNumerically("~", 0.0, 0.01))
		})

		It("clamps negative hit rate to 0.0", func() {
			shape, _ := tracker.Observe(5000, 200, -0.3)
			Expect(shape.PrefixHitRate).To(Equal(0.0))
			Expect(shape.ILeff).To(BeNumerically("~", 5000.0, 0.01))
		})
	})
})

// float64NaN returns a NaN value for use in tests.
func float64NaN() float64 {
	return math.NaN()
}
