package saturation_v2

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Rolling Average", func() {

	Describe("Add and Average", func() {
		It("should compute the mean of added values", func() {
			ra := newRollingAverage(5)
			ra.Add(10)
			ra.Add(20)
			ra.Add(30)

			Expect(ra.Average()).To(Equal(20.0))
			Expect(ra.Len()).To(Equal(3))
		})
	})

	Describe("Window eviction", func() {
		It("should evict oldest values when window is full", func() {
			ra := newRollingAverage(3)
			ra.Add(1)
			ra.Add(2)
			ra.Add(3)
			ra.Add(4) // evicts 1
			ra.Add(5) // evicts 2

			Expect(ra.Average()).To(Equal(4.0)) // (3+4+5)/3
			Expect(ra.Len()).To(Equal(3))
		})
	})

	Describe("Empty average", func() {
		It("should return 0 for an empty rolling average", func() {
			ra := newRollingAverage(5)
			Expect(ra.Average()).To(Equal(0.0))
			Expect(ra.Len()).To(Equal(0))
		})
	})

	Describe("Single value", func() {
		It("should return the value itself as the average", func() {
			ra := newRollingAverage(5)
			ra.Add(42)

			Expect(ra.Average()).To(Equal(42.0))
			Expect(ra.Len()).To(Equal(1))
		})
	})

	Describe("Large window with overflow", func() {
		It("should retain only the last maxSize values", func() {
			ra := newRollingAverage(RollingAverageWindowSize)
			for i := 1; i <= 15; i++ {
				ra.Add(float64(i))
			}

			Expect(ra.Len()).To(Equal(RollingAverageWindowSize))
			// Window contains 6..15, average = 10.5
			Expect(ra.Average()).To(BeNumerically("~", 10.5, 0.001))
		})
	})
})
