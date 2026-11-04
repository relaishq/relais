//go:build !darwin

package clusterprocess

func prepareParentReaper()  {}
func recordParentChild(int) {}
