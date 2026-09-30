package main

import (
	"net"
	"time"

	"k8s.io/client-go/rest"
)

const (
	leaderElectionQPS   = 5
	leaderElectionBurst = 10
)

func leaderElectionRestConfig(base *rest.Config) *rest.Config {
	cfg := rest.CopyConfig(base)
	cfg.RateLimiter = nil
	cfg.QPS = leaderElectionQPS
	cfg.Burst = leaderElectionBurst
	cfg.Dial = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
	return cfg
}
