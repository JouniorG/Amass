// Copyright 2017 Jeff Foley. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package amass

import (
	"errors"
	"io/ioutil"
	"log"
	"time"

	"github.com/OWASP/Amass/amass/core"
	"github.com/OWASP/Amass/amass/handlers"
	"github.com/OWASP/Amass/amass/sources"
	"github.com/OWASP/Amass/amass/utils"
	"github.com/google/uuid"
)

// Banner is the ASCII art logo used within help output.
var Banner = `

        .+++:.            :                             .+++.                   
      +W@@@@@@8        &+W@#               o8W8:      +W@@@@@@#.   oW@@@W#+     
     &@#+   .o@##.    .@@@o@W.o@@o       :@@#&W8o    .@#:  .:oW+  .@#+++&#&     
    +@&        &@&     #@8 +@W@&8@+     :@W.   +@8   +@:          .@8           
    8@          @@     8@o  8@8  WW    .@W      W@+  .@W.          o@#:         
    WW          &@o    &@:  o@+  o@+   #@.      8@o   +W@#+.        +W@8:       
    #@          :@W    &@+  &@+   @8  :@o       o@o     oW@@W+        oW@8      
    o@+          @@&   &@+  &@+   #@  &@.      .W@W       .+#@&         o@W.    
     WW         +@W@8. &@+  :&    o@+ #@      :@W&@&         &@:  ..     :@o    
     :@W:      o@# +Wo &@+        :W: +@W&o++o@W. &@&  8@#o+&@W.  #@:    o@+    
      :W@@WWWW@@8       +              :&W@@@@&    &W  .o#@@W&.   :W@WWW@@&     
        +o&&&&+.                                                    +oooo.      

`

const (
	// Version is used to display the current version of Amass.
	Version = "2.9.1"

	// Author is used to display the founder of the amass package.
	Author = "Jeff Foley - @jeff_foley"
)

// Enumeration is the object type used to execute a DNS enumeration with Amass.
type Enumeration struct {
	Config *core.Config

	Bus *core.EventBus

	// Link graph that collects all the information gathered by the enumeration
	Graph handlers.DataHandler

	// The channel that will receive the results
	Output chan *core.Output

	// Broadcast channel that indicates no further writes to the output channel
	Done chan struct{}

	dataSources []core.Service

	// Pause/Resume channels for halting the enumeration
	pause  chan struct{}
	resume chan struct{}

	filter      *utils.StringFilter
	outputQueue *utils.Queue
}

// NewEnumeration returns an initialized Enumeration that has not been started yet.
func NewEnumeration() *Enumeration {
	e := &Enumeration{
		Config: &core.Config{
			UUID: uuid.New(),
			Log:  log.New(ioutil.Discard, "", 0),
		},
		Bus:         core.NewEventBus(),
		Graph:       handlers.NewGraph(),
		Output:      make(chan *core.Output, 100),
		Done:        make(chan struct{}, 2),
		pause:       make(chan struct{}, 2),
		resume:      make(chan struct{}, 2),
		filter:      utils.NewStringFilter(),
		outputQueue: utils.NewQueue(),
	}
	e.dataSources = sources.GetAllSources(e.Config, e.Bus)
	return e
}

// Start begins the DNS enumeration process for the Amass Enumeration object.
func (e *Enumeration) Start() error {
	if e.Output == nil {
		return errors.New("The enumeration did not have an output channel")
	} else if e.Config.Passive && e.Config.DataOptsWriter != nil {
		return errors.New("Data operations cannot be saved without DNS resolution")
	} else if err := e.Config.CheckSettings(); err != nil {
		return err
	}

	if e.Config.GremlinURL != "" {
		gremlin := handlers.NewGremlin(e.Config.GremlinURL,
			e.Config.GremlinUser, e.Config.GremlinPass, e.Config.Log)
		e.Graph = gremlin
		defer gremlin.Close()
	}

	e.Bus.Subscribe(core.OutputTopic, e.sendOutput)

	if len(e.Config.DisabledDataSources) > 0 {
		e.dataSources = e.Config.ExcludeDisabledDataSources(e.dataSources)
	}

	// Select the correct services to be used in this enumeration
	var services []core.Service
	if !e.Config.Passive {
		dms := NewDataManagerService(e.Config, e.Bus)
		dms.AddDataHandler(e.Graph)
		if e.Config.DataOptsWriter != nil {
			dms.AddDataHandler(handlers.NewDataOptsHandler(e.Config.DataOptsWriter))
		}
		services = append(services, NewDNSService(e.Config, e.Bus), dms, NewActiveCertService(e.Config, e.Bus))
	}

	namesrv := NewNameService(e.Config, e.Bus)
	namesrv.RegisterGraph(e.Graph)
	services = append(services, namesrv, NewAddressService(e.Config, e.Bus))
	if !e.Config.Passive {
		services = append(services, NewAlterationService(e.Config, e.Bus), NewBruteForceService(e.Config, e.Bus))
	}

	// Grab all the data sources
	services = append(services, e.dataSources...)
	for _, srv := range services {
		if err := srv.Start(); err != nil {
			return err
		}
	}

	t := time.NewTicker(3 * time.Second)
	out := time.NewTicker(5 * time.Second)
	go e.processOutput()
loop:
	for {
		select {
		case <-e.Done:
			break loop
		case <-e.PauseChan():
			t.Stop()
			out.Stop()
		case <-e.ResumeChan():
			t = time.NewTicker(3 * time.Second)
			out = time.NewTicker(time.Second)
		case <-out.C:
			e.checkForOutput()
		case <-t.C:
			done := true
			for _, srv := range services {
				if srv.IsActive() {
					done = false
					break
				}
			}
			if done {
				close(e.Done)
			}
		}
	}
	t.Stop()
	out.Stop()
	for _, srv := range services {
		srv.Stop()
	}
	return nil
}

func (e *Enumeration) processOutput() {
	curIdx := 0
	maxIdx := 7
	delays := []int{250, 500, 750, 1000, 1250, 1500, 1750, 2000}
loop:
	for {
		select {
		case <-e.Done:
			break loop
		default:
			element, ok := e.outputQueue.Next()
			if !ok {
				if curIdx < maxIdx {
					curIdx++
				}
				time.Sleep(time.Duration(delays[curIdx]) * time.Millisecond)
				continue
			}
			curIdx = 0
			e.Output <- element.(*core.Output)
		}
	}
	close(e.Output)
}

func (e *Enumeration) checkForOutput() {
	select {
	case <-e.Done:
		return
	default:
		if out := e.Graph.GetUnreadOutput(e.Config.UUID.String()); len(out) > 0 {
			for _, o := range out {
				if time.Now().Add(10*time.Second).After(o.Timestamp) && !e.filter.Duplicate(o.Name) {
					e.Graph.MarkAsRead(&handlers.DataOptsParams{
						UUID:   e.Config.UUID.String(),
						Name:   o.Name,
						Domain: o.Domain,
					})

					if e.Config.IsDomainInScope(o.Name) {
						e.outputQueue.Append(o)
					}
				}
			}
		}
	}
}

func (e *Enumeration) sendOutput(o *core.Output) {
	select {
	case <-e.Done:
		return
	default:
		if !e.filter.Duplicate(o.Name) && e.Config.IsDomainInScope(o.Name) {
			e.outputQueue.Append(o)
		}
	}
}

// Pause temporarily halts the enumeration.
func (e *Enumeration) Pause() {
	e.pause <- struct{}{}
}

// PauseChan returns the channel that is signaled when Pause is called.
func (e *Enumeration) PauseChan() <-chan struct{} {
	return e.pause
}

// Resume causes a previously paused enumeration to resume execution.
func (e *Enumeration) Resume() {
	e.resume <- struct{}{}
}

// ResumeChan returns the channel that is signaled when Resume is called.
func (e *Enumeration) ResumeChan() <-chan struct{} {
	return e.resume
}

// TrustedTag returns true when the tag parameter is of a type that should be trusted even
// facing DNS wildcards.
func TrustedTag(tag string) bool {
	if tag == core.DNS || tag == core.CERT || tag == core.ARCHIVE || tag == core.AXFR {
		return true
	}
	return false
}

// GetAllSourceNames returns the names of all the available data sources.
func (e *Enumeration) GetAllSourceNames() []string {
	var names []string

	for _, source := range e.dataSources {
		names = append(names, source.String())
	}
	return names
}
