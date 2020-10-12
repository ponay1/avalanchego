package xput

// Tester runs throughput tests
type Tester interface {
	// Run the test and return results (e.g. statistics)
	Run(config interface{}) (interface{}, error)
}
