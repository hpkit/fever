package processing

// DCSO FEVER
// Copyright (c) 2017, DCSO GmbH

import (
	"net"
	"sync"
	"time"

	"github.com/DCSO/fever/types"
	"github.com/DCSO/fever/util"

	log "github.com/sirupsen/logrus"
)

// ForwardHandlerPerfStats contains performance stats written to InfluxDB
// for monitoring.
type ForwardHandlerPerfStats struct {
	ForwardedPerSec uint64 `influx:"forwarded_events_per_sec"`
}

// ForwardHandler is a handler that processes events by writing their JSON
// representation into a UNIX socket. This is limited by a list of allowed
// event types to be forwarded.
type ForwardHandler struct {
	Logger              *log.Entry
	ForwardEventChan    chan []byte
	OutputSocket        string
	OutputConn          net.Conn
	Reconnecting        bool
	ReconnLock          sync.Mutex
	ReconnectNotifyChan chan bool
	StopReconnectChan   chan bool
	ReconnectTimes      int
	PerfStats           ForwardHandlerPerfStats
	StatsEncoder        *util.PerformanceStatsEncoder
	StopChan            chan bool
	StoppedChan         chan bool
	Running             bool
	Lock                sync.Mutex
}

func (fh *ForwardHandler) reconnectForward() {
	for range fh.ReconnectNotifyChan {
		var i int
		log.Info("Reconnecting to forwarding socket...")
		outputConn, myerror := net.Dial("unix", fh.OutputSocket)
		fh.ReconnLock.Lock()
		if !fh.Reconnecting {
			fh.Reconnecting = true
		} else {
			fh.ReconnLock.Unlock()
			continue
		}
		fh.ReconnLock.Unlock()
		for i = 0; (fh.ReconnectTimes == 0 || i < fh.ReconnectTimes) && myerror != nil; i++ {
			select {
			case <-fh.StopReconnectChan:
				return
			default:
				log.WithFields(log.Fields{
					"domain":     "forward",
					"retry":      i + 1,
					"maxretries": fh.ReconnectTimes,
				}).Warnf("error connecting to output socket, retrying: %s", myerror)
				time.Sleep(10 * time.Second)
				outputConn, myerror = net.Dial("unix", fh.OutputSocket)
			}
		}
		if myerror != nil {
			log.WithFields(log.Fields{
				"domain":  "forward",
				"retries": i,
			}).Fatalf("permanent error connecting to output socket: %s", myerror)
		} else {
			if i > 0 {
				log.WithFields(log.Fields{
					"domain":         "forward",
					"retry_attempts": i,
				}).Infof("connection to output socket successful")
			}
			fh.Lock.Lock()
			fh.OutputConn = outputConn
			fh.Lock.Unlock()
			fh.ReconnLock.Lock()
			fh.Reconnecting = false
			fh.ReconnLock.Unlock()
		}
	}
}

func (fh *ForwardHandler) runForward() {
	var err error
	for {
		select {
		case <-fh.StopChan:
			close(fh.StoppedChan)
			return
		default:
			for item := range fh.ForwardEventChan {
				select {
				case <-fh.StopChan:
					close(fh.StoppedChan)
					return
				default:
					fh.ReconnLock.Lock()
					if fh.Reconnecting {
						fh.ReconnLock.Unlock()
						continue
					}
					fh.ReconnLock.Unlock()
					fh.Lock.Lock()
					if fh.OutputConn != nil {
						_, err = fh.OutputConn.Write(item)
						if err != nil {
							fh.OutputConn.Close()
							log.Warn(err)
							fh.ReconnectNotifyChan <- true
							fh.Lock.Unlock()
							continue
						}
						_, err = fh.OutputConn.Write([]byte("\n"))
						if err != nil {
							fh.OutputConn.Close()
							log.Warn(err)
							fh.Lock.Unlock()
							continue
						}
					}
					fh.Lock.Unlock()
				}
			}
		}
	}
}

func (fh *ForwardHandler) runCounter() {
	var nofSecs uint64 = 10
	for {
		select {
		case <-fh.StopChan:
			return
		default:
			time.Sleep(time.Duration(nofSecs) * time.Second)
			fh.Lock.Lock()
			if fh.StatsEncoder != nil {
				fh.PerfStats.ForwardedPerSec /= nofSecs
				fh.StatsEncoder.Submit(fh.PerfStats)
			}
			fh.PerfStats.ForwardedPerSec = 0
			fh.Lock.Unlock()

		}
	}
}

// MakeForwardHandler creates a new forwarding handler
func MakeForwardHandler(reconnectTimes int, outputSocket string) *ForwardHandler {
	fh := &ForwardHandler{
		Logger: log.WithFields(log.Fields{
			"domain": "forward",
		}),
		OutputSocket:        outputSocket,
		ReconnectTimes:      reconnectTimes,
		ReconnectNotifyChan: make(chan bool),
		StopReconnectChan:   make(chan bool),
	}
	return fh
}

// Consume processes an Entry and forwards it
func (fh *ForwardHandler) Consume(e *types.Entry) error {
	doForwardThis := util.ForwardAllEvents || util.AllowType(e.EventType)
	if doForwardThis {
		jsonCopy := make([]byte, len(e.JSONLine))
		copy(jsonCopy, e.JSONLine)
		fh.ForwardEventChan <- jsonCopy
		fh.Lock.Lock()
		fh.PerfStats.ForwardedPerSec++
		fh.Lock.Unlock()
	}
	return nil
}

// GetName returns the name of the handler
func (fh *ForwardHandler) GetName() string {
	return "Forwarding handler"
}

// GetEventTypes returns a slice of event type strings that this handler
// should be applied to
func (fh *ForwardHandler) GetEventTypes() []string {
	if util.ForwardAllEvents {
		return []string{"*"}
	}
	return util.GetAllowedTypes()
}

// Run starts forwarding of JSON representations of all consumed events
func (fh *ForwardHandler) Run() {
	if !fh.Running {
		fh.StopChan = make(chan bool)
		fh.ForwardEventChan = make(chan []byte, 10000)
		go fh.reconnectForward()
		fh.ReconnectNotifyChan <- true
		go fh.runForward()
		go fh.runCounter()
		fh.Running = true
	}
}

// Stop stops forwarding of JSON representations of all consumed events
func (fh *ForwardHandler) Stop(stoppedChan chan bool) {
	if fh.Running {
		fh.StoppedChan = stoppedChan
		fh.Lock.Lock()
		fh.OutputConn.Close()
		fh.Lock.Unlock()
		close(fh.StopReconnectChan)
		close(fh.ReconnectNotifyChan)
		close(fh.StopChan)
		close(fh.ForwardEventChan)
		fh.Running = false
	}
}

// SubmitStats registers a PerformanceStatsEncoder for runtime stats submission.
func (fh *ForwardHandler) SubmitStats(sc *util.PerformanceStatsEncoder) {
	fh.StatsEncoder = sc
}
