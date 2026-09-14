package prometheus

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("FormatPrometheusDuration", func() {
	DescribeTable("formats durations correctly",
		func(duration time.Duration, expected string) {
			result := FormatPrometheusDuration(duration)
			Expect(result).To(Equal(expected))
		},
		// Days
		Entry("1 day", 24*time.Hour, "1d"),
		Entry("2 days", 48*time.Hour, "2d"),
		Entry("7 days", 7*24*time.Hour, "7d"),

		// Hours
		Entry("1 hour", 1*time.Hour, "1h"),
		Entry("12 hours", 12*time.Hour, "12h"),

		// Minutes
		Entry("1 minute", 1*time.Minute, "1m"),
		Entry("10 minutes", 10*time.Minute, "10m"),
		Entry("30 minutes", 30*time.Minute, "30m"),

		// Seconds
		Entry("1 second", 1*time.Second, "1s"),
		Entry("30 seconds", 30*time.Second, "30s"),

		// Mixed durations (not cleanly divisible)
		Entry("90 minutes", 90*time.Minute, "90m"),

		// Very short durations
		Entry("100 milliseconds", 100*time.Millisecond, "1s"),
		Entry("zero", time.Duration(0), "1s"),
	)
})

var _ = Describe("CategorizePrometheusError", func() {
	Context("when error is nil", func() {
		It("should return empty string", func() {
			result := CategorizePrometheusError(nil)
			Expect(result).To(Equal(""))
		})
	})

	Context("when error is a context error", func() {
		It("should return 'timeout' for context.DeadlineExceeded", func() {
			result := CategorizePrometheusError(context.DeadlineExceeded)
			Expect(result).To(Equal("timeout"))
		})

		It("should return 'canceled' for context.Canceled", func() {
			result := CategorizePrometheusError(context.Canceled)
			Expect(result).To(Equal("canceled"))
		})

		It("should return 'timeout' for wrapped context.DeadlineExceeded", func() {
			wrappedErr := errors.Join(errors.New("query failed"), context.DeadlineExceeded)
			result := CategorizePrometheusError(wrappedErr)
			Expect(result).To(Equal("timeout"))
		})
	})

	DescribeTable("categorizes error messages correctly",
		func(errMsg string, expected string) {
			err := errors.New(errMsg)
			result := CategorizePrometheusError(err)
			Expect(result).To(Equal(expected))
		},
		// Connection errors
		Entry("connection refused", "connection refused", "connection_refused"),
		Entry("connection refused uppercase", "Connection Refused", "connection_refused"),
		Entry("dial tcp: connection refused", "dial tcp 127.0.0.1:9090: connection refused", "connection_refused"),

		// DNS errors
		Entry("no such host", "no such host", "dns_error"),
		Entry("no such host uppercase", "No Such Host", "dns_error"),
		Entry("lookup error", "lookup prometheus.example.com: no such host", "dns_error"),

		// Timeout errors (in message)
		Entry("timeout in message", "request timeout", "timeout"),
		Entry("timeout uppercase", "Request Timeout", "timeout"),
		Entry("i/o timeout", "i/o timeout", "timeout"),

		// Parse errors
		Entry("parse error", "parse error: invalid query", "parse_error"),
		Entry("parse error uppercase", "Parse Error", "parse_error"),

		// Bad data
		Entry("bad_data", "bad_data: query result is invalid", "bad_data"),
		Entry("bad_data uppercase", "Bad_Data", "bad_data"),

		// Execution errors
		Entry("execution error", "execution error: out of memory", "execution_error"),
		Entry("execution uppercase", "Execution failed", "execution_error"),

		// Query processing errors
		Entry("query processing", "query processing failed", "query_processing"),
		Entry("query processing uppercase", "Query Processing Error", "query_processing"),

		// Unknown errors
		Entry("unknown error", "something went wrong", "unknown"),
		Entry("random error", "unexpected error occurred", "unknown"),
		Entry("generic error", "error", "unknown"),
	)
})
