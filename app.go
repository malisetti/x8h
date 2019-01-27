package main

import "sync"

type app struct {
	sync.Mutex
	lq *limitQueue
}
