// Package errors defines typed errors used in cluster state-machine decisions.
package errors

import "errors"

type splitBrainRisk struct{}

func (splitBrainRisk) Error() string { return "split-brain risk: peers reachable but join failed" }

type noPeersReachable struct{}

func (noPeersReachable) Error() string { return "no peers reachable" }

func NewSplitBrainRisk() error   { return splitBrainRisk{} }
func NewNoPeersReachable() error { return noPeersReachable{} }

func IsSplitBrainRisk(err error) bool {
var s splitBrainRisk
return errors.As(err, &s)
}

func IsNoPeersReachable(err error) bool {
var n noPeersReachable
return errors.As(err, &n)
}
