package main

import "sync"

type app struct {
	sync.Mutex
	LimitQueue *limitQueue `json:"limitQueue"`
	VisitCount int         `json:"visitCount"`
}
