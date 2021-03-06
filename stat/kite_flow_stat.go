package stat

import (
	"fmt"
	"log"
	"sync/atomic"
	"time"
)

type FlowControl struct {
	name           string
	ReadFlow       *flow
	DispatcherFlow *flow
	WriteFlow      *flow
	stop           bool
}

func NewFlowControl(name string) *FlowControl {
	return &FlowControl{
		name:           name,
		ReadFlow:       &flow{},
		DispatcherFlow: &flow{},
		WriteFlow:      &flow{},
		stop:           false}
}

func (self *FlowControl) Start() {

	go func() {
		for !self.stop {
			line := fmt.Sprintf("%s:\tread:%d\tdispatcher:%d\twrite:%d", self.name, self.ReadFlow.changes(),
				self.DispatcherFlow.changes(), self.WriteFlow.changes())
			log.Println(line)
			time.Sleep(1 * time.Second)
		}
	}()
}

func (self *FlowControl) Stop() {
	self.stop = true
}

type flow struct {
	count     int32
	lastcount int32
}

func (self *flow) Incr(num int32) {
	atomic.AddInt32(&self.count, num)
}

func (self *flow) changes() int32 {
	tmpc := self.count
	tmpl := self.lastcount
	c := tmpc - tmpl
	self.lastcount = tmpc
	return c
}
