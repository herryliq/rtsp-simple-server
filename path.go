package main

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aler9/gortsplib"

	"github.com/aler9/rtsp-simple-server/conf"
	"github.com/aler9/rtsp-simple-server/externalcmd"
	"github.com/aler9/rtsp-simple-server/serverudp"
	"github.com/aler9/rtsp-simple-server/sourcertmp"
	"github.com/aler9/rtsp-simple-server/sourcertsp"
	"github.com/aler9/rtsp-simple-server/stats"
)

const (
	pathCheckPeriod                    = 5 * time.Second
	describeTimeout                    = 5 * time.Second
	sourceStopAfterDescribePeriod      = 10 * time.Second
	onDemandCmdStopAfterDescribePeriod = 10 * time.Second
)

type LogFunc func(string, ...interface{})

type OnCloseFunc func(*path)

// a source can be a client, a sourcertsp.Source or a sourcertmp.Source
type source interface {
	IsSource()
}

type path struct {
	wg                     sync.WaitGroup
	log                    LogFunc
	stats                  *stats.Stats
	serverUdpRtp           *serverudp.Server
	serverUdpRtcp          *serverudp.Server
	readTimeout            time.Duration
	writeTimeout           time.Duration
	name                   string
	conf                   *conf.PathConf
	clients                map[*client]struct{}
	source                 source
	sourceReady            bool
	sourceTrackCount       int
	sourceSdp              []byte
	lastDescribeReq        time.Time
	lastDescribeActivation time.Time
	readers                *ReadersMap
	onInitCmd              *externalcmd.ExternalCmd
	onDemandCmd            *externalcmd.ExternalCmd
	onClose                OnCloseFunc

	sourceSetReady    chan struct{}
	sourceSetNotReady chan struct{}
	clientDescribe    chan clientDescribeReq
	clientAnnounce    chan clientAnnounceReq
	clientSetupPlay   chan clientSetupPlayReq
	clientPlay        chan *client
	clientRecord      chan *client
	clientClose       chan *client
	terminate         chan struct{}
}

func newPath(wg sync.WaitGroup,
	log LogFunc,
	stats *stats.Stats,
	serverUdpRtp *serverudp.Server,
	serverUdpRtcp *serverudp.Server,
	readTimeout time.Duration,
	writeTimeout time.Duration,
	name string,
	conf *conf.PathConf,
	onClose OnCloseFunc) *path {
	pa := &path{
		wg: wg,
		log: func(format string, args ...interface{}) {
			log("[path "+name+"] "+format, args...)
		},
		stats:             stats,
		serverUdpRtp:      serverUdpRtp,
		serverUdpRtcp:     serverUdpRtcp,
		readTimeout:       readTimeout,
		writeTimeout:      writeTimeout,
		name:              name,
		conf:              conf,
		clients:           make(map[*client]struct{}),
		readers:           NewReadersMap(),
		onClose:           onClose,
		sourceSetReady:    make(chan struct{}),
		sourceSetNotReady: make(chan struct{}),
		clientDescribe:    make(chan clientDescribeReq),
		clientAnnounce:    make(chan clientAnnounceReq),
		clientSetupPlay:   make(chan clientSetupPlayReq),
		clientPlay:        make(chan *client),
		clientRecord:      make(chan *client),
		clientClose:       make(chan *client),
		terminate:         make(chan struct{}),
	}

	pa.wg.Add(1)
	go pa.run()
	return pa
}

func (pa *path) run() {
	defer pa.wg.Done()

	if strings.HasPrefix(pa.conf.Source, "rtsp://") {
		state := sourcertsp.StateStopped
		if !pa.conf.SourceOnDemand {
			state = sourcertsp.StateRunning
		}

		s := sourcertsp.New(
			pa.conf.Source,
			pa.conf.SourceProtocolParsed,
			sourcertsp.LogFunc(pa.log),
			pa.readTimeout,
			pa.writeTimeout,
			state,
			func(s *sourcertsp.Source, tracks gortsplib.Tracks) {
				pa.sourceSdp = tracks.Write()
				pa.sourceTrackCount = len(tracks)
				pa.sourceSetReady <- struct{}{}
			},
			func(s *sourcertsp.Source) {
				pa.sourceSetNotReady <- struct{}{}
			},
			pa.readers.ForwardFrame)
		pa.source = s

		atomic.AddInt64(pa.stats.CountSourcesRtsp, +1)
		if !pa.conf.SourceOnDemand {
			atomic.AddInt64(pa.stats.CountSourcesRtspRunning, +1)
		}

	} else if strings.HasPrefix(pa.conf.Source, "rtmp://") {
		state := sourcertmp.StateStopped
		if !pa.conf.SourceOnDemand {
			state = sourcertmp.StateRunning
		}

		s := sourcertmp.New(
			pa.conf.Source,
			sourcertmp.LogFunc(pa.log),
			state,
			func(s *sourcertmp.Source, tracks gortsplib.Tracks) {
				pa.sourceSdp = tracks.Write()
				pa.sourceTrackCount = len(tracks)
				pa.sourceSetReady <- struct{}{}
			},
			func(s *sourcertmp.Source) {
				pa.sourceSetNotReady <- struct{}{}
			},
			pa.readers.ForwardFrame)
		pa.source = s

		atomic.AddInt64(pa.stats.CountSourcesRtmp, +1)
		if !pa.conf.SourceOnDemand {
			atomic.AddInt64(pa.stats.CountSourcesRtmpRunning, +1)
		}
	}

	if pa.conf.RunOnInit != "" {
		pa.log("starting on init command")

		var err error
		pa.onInitCmd, err = externalcmd.New(pa.conf.RunOnInit, pa.name)
		if err != nil {
			pa.log("ERR: %s", err)
		}
	}

	tickerCheck := time.NewTicker(pathCheckPeriod)
	defer tickerCheck.Stop()

outer:
	for {
		select {
		case <-tickerCheck.C:
			ok := pa.onCheck()
			if !ok {
				pa.onClose(pa)
				<-pa.terminate
				break outer
			}

		case <-pa.sourceSetReady:
			pa.onSourceSetReady()

		case <-pa.sourceSetNotReady:
			pa.onSourceSetNotReady()

		case req := <-pa.clientDescribe:
			pa.onClientDescribe(req.client)

		case req := <-pa.clientSetupPlay:
			err := pa.onClientSetupPlay(req.client, req.trackId)
			req.res <- err

		case c := <-pa.clientPlay:
			atomic.AddInt64(pa.stats.CountReaders, 1)
			pa.onClientPlay(c)

		case req := <-pa.clientAnnounce:
			err := pa.onClientAnnounce(req.client, req.tracks)
			req.res <- err

		case c := <-pa.clientRecord:
			pa.onClientRecord(c)

		case c := <-pa.clientClose:
			pa.onClientClose(c)

		case <-pa.terminate:
			break outer
		}
	}

	if pa.onInitCmd != nil {
		pa.log("stopping on init command (closing)")
		pa.onInitCmd.Close()
	}

	if source, ok := pa.source.(*sourcertsp.Source); ok {
		source.Close()

	} else if source, ok := pa.source.(*sourcertmp.Source); ok {
		source.Close()
	}

	if pa.onDemandCmd != nil {
		pa.log("stopping on demand command (closing)")
		pa.onDemandCmd.Close()
	}

	for c := range pa.clients {
		if c.state == clientStateWaitDescription {
			delete(pa.clients, c)
			c.path = nil
			c.state = clientStateInitial
			c.describe <- describeRes{nil, fmt.Errorf("publisher of path '%s' has timed out", pa.name)}
		} else {
			c.close()
		}
	}

	close(pa.clientDescribe)
}

func (pa *path) close() {
	close(pa.terminate)
}

func (pa *path) hasClients() bool {
	return len(pa.clients) > 0
}

func (pa *path) hasClientsWaitingDescribe() bool {
	for c := range pa.clients {
		if c.state == clientStateWaitDescription {
			return true
		}
	}
	return false
}

func (pa *path) hasClientReadersOrWaitingDescribe() bool {
	for c := range pa.clients {
		if c != pa.source {
			return true
		}
	}
	return false
}

func (pa *path) onCheck() bool {
	// reply to DESCRIBE requests if they are in timeout
	if pa.hasClientsWaitingDescribe() &&
		time.Since(pa.lastDescribeActivation) >= describeTimeout {
		for c := range pa.clients {
			if c.state == clientStateWaitDescription {
				delete(pa.clients, c)
				c.path = nil
				c.state = clientStateInitial
				c.describe <- describeRes{nil, fmt.Errorf("publisher of path '%s' has timed out", pa.name)}
			}
		}
	}

	// stop on demand rtsp source if needed
	if source, ok := pa.source.(*sourcertsp.Source); ok {
		if pa.conf.SourceOnDemand &&
			source.State() == sourcertsp.StateRunning &&
			!pa.hasClients() &&
			time.Since(pa.lastDescribeReq) >= sourceStopAfterDescribePeriod {
			pa.log("stopping on demand rtsp source (not requested anymore)")
			atomic.AddInt64(pa.stats.CountSourcesRtspRunning, -1)
			source.SetState(sourcertsp.StateStopped)
		}

		// stop on demand rtmp source if needed
	} else if source, ok := pa.source.(*sourcertmp.Source); ok {
		if pa.conf.SourceOnDemand &&
			source.State() == sourcertmp.StateRunning &&
			!pa.hasClients() &&
			time.Since(pa.lastDescribeReq) >= sourceStopAfterDescribePeriod {
			pa.log("stopping on demand rtmp source (not requested anymore)")
			atomic.AddInt64(pa.stats.CountSourcesRtmpRunning, -1)
			source.SetState(sourcertmp.StateStopped)
		}
	}

	// stop on demand command if needed
	if pa.onDemandCmd != nil &&
		!pa.hasClientReadersOrWaitingDescribe() &&
		time.Since(pa.lastDescribeReq) >= onDemandCmdStopAfterDescribePeriod {
		pa.log("stopping on demand command (not requested anymore)")
		pa.onDemandCmd.Close()
		pa.onDemandCmd = nil
	}

	// remove path if is regexp and has no clients
	if pa.conf.Regexp != nil &&
		pa.source == nil &&
		!pa.hasClients() {
		return false
	}

	return true
}

func (pa *path) onSourceSetReady() {
	pa.sourceReady = true

	// reply to all clients that are waiting for a description
	for c := range pa.clients {
		if c.state == clientStateWaitDescription {
			delete(pa.clients, c)
			c.path = nil
			c.state = clientStateInitial
			c.describe <- describeRes{pa.sourceSdp, nil}
		}
	}
}

func (pa *path) onSourceSetNotReady() {
	pa.sourceReady = false

	// close all clients that are reading or waiting to read
	for c := range pa.clients {
		if c.state != clientStateWaitDescription && c != pa.source {
			c.close()
		}
	}
}

func (pa *path) onClientDescribe(c *client) {
	pa.lastDescribeReq = time.Now()

	// publisher not found
	if pa.source == nil {
		// on demand command is available: put the client on hold
		if pa.conf.RunOnDemand != "" {
			if pa.onDemandCmd == nil { // start if needed
				pa.log("starting on demand command")
				pa.lastDescribeActivation = time.Now()

				var err error
				pa.onDemandCmd, err = externalcmd.New(pa.conf.RunOnDemand, pa.name)
				if err != nil {
					pa.log("ERR: %s", err)
				}
			}

			pa.clients[c] = struct{}{}
			c.path = pa
			c.state = clientStateWaitDescription

			// no on-demand: reply with 404
		} else {
			c.describe <- describeRes{nil, fmt.Errorf("no one is publishing on path '%s'", pa.name)}
		}

		// publisher was found but is not ready: put the client on hold
	} else if !pa.sourceReady {
		// start rtsp source if needed
		if source, ok := pa.source.(*sourcertsp.Source); ok {
			if source.State() == sourcertsp.StateStopped {
				pa.log("starting on demand rtsp source")
				pa.lastDescribeActivation = time.Now()
				atomic.AddInt64(pa.stats.CountSourcesRtspRunning, +1)
				source.SetState(sourcertsp.StateRunning)
			}

			// start rtmp source if needed
		} else if source, ok := pa.source.(*sourcertmp.Source); ok {
			if source.State() == sourcertmp.StateStopped {
				pa.log("starting on demand rtmp source")
				pa.lastDescribeActivation = time.Now()
				atomic.AddInt64(pa.stats.CountSourcesRtmpRunning, +1)
				source.SetState(sourcertmp.StateRunning)
			}
		}

		pa.clients[c] = struct{}{}
		c.path = pa
		c.state = clientStateWaitDescription

		// publisher was found and is ready
	} else {
		c.describe <- describeRes{pa.sourceSdp, nil}
	}
}

func (pa *path) onClientSetupPlay(c *client, trackId int) error {
	if !pa.sourceReady {
		return fmt.Errorf("no one is publishing on path '%s'", pa.name)
	}

	if trackId >= pa.sourceTrackCount {
		return fmt.Errorf("track %d does not exist", trackId)
	}

	pa.clients[c] = struct{}{}
	c.path = pa
	c.state = clientStatePrePlay

	return nil
}

func (pa *path) onClientPlay(c *client) {
	c.state = clientStatePlay
	pa.readers.Add(c)
}

func (pa *path) onClientAnnounce(c *client, tracks gortsplib.Tracks) error {
	if pa.source != nil {
		return fmt.Errorf("someone is already publishing on path '%s'", pa.name)
	}

	pa.clients[c] = struct{}{}
	pa.source = c
	pa.sourceTrackCount = len(tracks)
	pa.sourceSdp = tracks.Write()

	c.path = pa
	c.state = clientStatePreRecord

	return nil
}

func (pa *path) onClientRecord(c *client) {
	atomic.AddInt64(pa.stats.CountPublishers, 1)
	c.state = clientStateRecord

	if c.streamProtocol == gortsplib.StreamProtocolUDP {
		for trackId, track := range c.streamTracks {
			addr := serverudp.MakePublisherAddr(c.ip(), track.rtpPort)
			pa.serverUdpRtp.AddPublisher(addr, c, trackId)

			addr = serverudp.MakePublisherAddr(c.ip(), track.rtcpPort)
			pa.serverUdpRtcp.AddPublisher(addr, c, trackId)
		}
	}

	pa.onSourceSetReady()
}

func (pa *path) onClientClose(c *client) {
	delete(pa.clients, c)

	switch c.state {
	case clientStatePlay:
		atomic.AddInt64(pa.stats.CountReaders, -1)
		pa.readers.Remove(c)

	case clientStateRecord:
		atomic.AddInt64(pa.stats.CountPublishers, -1)

		if c.streamProtocol == gortsplib.StreamProtocolUDP {
			for _, track := range c.streamTracks {
				addr := serverudp.MakePublisherAddr(c.ip(), track.rtpPort)
				pa.serverUdpRtp.RemovePublisher(addr)

				addr = serverudp.MakePublisherAddr(c.ip(), track.rtcpPort)
				pa.serverUdpRtcp.RemovePublisher(addr)
			}
		}

		pa.onSourceSetNotReady()
	}

	if pa.source == c {
		pa.source = nil

		// close all clients that are reading or waiting to read
		for oc := range pa.clients {
			if oc.state != clientStateWaitDescription && oc != pa.source {
				oc.close()
			}
		}
	}
}
