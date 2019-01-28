package main

import (
	"sync"

	"github.com/oschwald/geoip2-golang"
)

type app struct {
	sync.Mutex
	geoIPReader *geoip2.Reader
	lq          *limitQueue
}
