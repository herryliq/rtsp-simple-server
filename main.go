package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aler9/gortsplib"
	"gopkg.in/alecthomas/kingpin.v2"

	"github.com/aler9/rtsp-simple-server/conf"
	"github.com/aler9/rtsp-simple-server/loghandler"
	"github.com/aler9/rtsp-simple-server/metrics"
	"github.com/aler9/rtsp-simple-server/pprof"
	"github.com/aler9/rtsp-simple-server/servertcp"
	"github.com/aler9/rtsp-simple-server/serverudp"
	"github.com/aler9/rtsp-simple-server/stats"
)

var Version = "v0.0.0"

const (
	checkPathPeriod = 5 * time.Second
)

type program struct {
	conf          *conf.Conf
	logHandler    *loghandler.LogHandler
	metrics       *metrics.Metrics
	pprof         *pprof.Pprof
	paths         map[string]*path
	serverUdpRtp  *serverudp.ServerUDP
	serverUdpRtcp *serverudp.ServerUDP
	serverTcp     *servertcp.ServerTCP
	clients       map[*client]struct{}
	clientsWg     sync.WaitGroup
	stats         *stats.Stats

	clientNew       chan net.Conn
	clientClose     chan *client
	clientDescribe  chan clientDescribeReq
	clientAnnounce  chan clientAnnounceReq
	clientSetupPlay chan clientSetupPlayReq
	clientPlay      chan *client
	clientRecord    chan *client
	sourceReady     chan *path
	sourceNotReady  chan *path
	terminate       chan struct{}
	done            chan struct{}
}

func newProgram(args []string) (*program, error) {
	k := kingpin.New("rtsp-simple-server",
		"rtsp-simple-server "+Version+"\n\nRTSP server.")

	argVersion := k.Flag("version", "print version").Bool()
	argConfPath := k.Arg("confpath", "path to a config file. The default is rtsp-simple-server.yml.").Default("rtsp-simple-server.yml").String()

	kingpin.MustParse(k.Parse(args))

	if *argVersion == true {
		fmt.Println(Version)
		os.Exit(0)
	}

	conf, err := conf.Load(*argConfPath)
	if err != nil {
		return nil, err
	}

	p := &program{
		conf:            conf,
		paths:           make(map[string]*path),
		clients:         make(map[*client]struct{}),
		stats:           stats.New(),
		clientNew:       make(chan net.Conn),
		clientClose:     make(chan *client),
		clientDescribe:  make(chan clientDescribeReq),
		clientAnnounce:  make(chan clientAnnounceReq),
		clientSetupPlay: make(chan clientSetupPlayReq),
		clientPlay:      make(chan *client),
		clientRecord:    make(chan *client),
		sourceReady:     make(chan *path),
		sourceNotReady:  make(chan *path),
		terminate:       make(chan struct{}),
		done:            make(chan struct{}),
	}

	p.logHandler, err = loghandler.New(conf.LogDestinationsParsed, conf.LogFile)
	if err != nil {
		return nil, err
	}

	p.log("rtsp-simple-server %s", Version)

	if conf.Metrics {
		p.metrics, err = metrics.New(p.log, p.stats)
		if err != nil {
			p.logHandler.Close()
			return nil, err
		}
	}

	if conf.Pprof {
		p.pprof, err = pprof.New(p.log)
		if err != nil {
			p.logHandler.Close()
			p.metrics.Close()
			return nil, err
		}
	}

	for name, pathConf := range conf.Paths {
		if pathConf.Regexp == nil {
			p.paths[name] = newPath(p, name, pathConf)
		}
	}

	if _, ok := conf.ProtocolsParsed[gortsplib.StreamProtocolUDP]; ok {
		p.serverUdpRtp, err = serverudp.New(p.log, p.conf.WriteTimeout, conf.RtpPort, gortsplib.StreamTypeRtp)
		if err != nil {
			p.pprof.Close()
			p.metrics.Close()
			p.logHandler.Close()
			return nil, err
		}

		p.serverUdpRtcp, err = serverudp.New(p.log, p.conf.WriteTimeout, conf.RtcpPort, gortsplib.StreamTypeRtcp)
		if err != nil {
			p.pprof.Close()
			p.metrics.Close()
			p.logHandler.Close()
			return nil, err
		}
	}

	p.serverTcp, err = servertcp.New(p.log, conf.RtspPort,
		func(c net.Conn) { p.clientNew <- c })
	if err != nil {
		p.pprof.Close()
		p.metrics.Close()
		p.logHandler.Close()
		if p.serverUdpRtp != nil {
			p.serverUdpRtp.Close()
		}
		if p.serverUdpRtcp != nil {
			p.serverUdpRtcp.Close()
		}
		return nil, err
	}

	go p.run()

	return p, nil
}

func (p *program) log(format string, args ...interface{}) {
	CountClients := atomic.LoadInt64(p.stats.CountClients)
	CountPublishers := atomic.LoadInt64(p.stats.CountPublishers)
	CountReaders := atomic.LoadInt64(p.stats.CountReaders)

	log.Printf(fmt.Sprintf("[%d/%d/%d] "+format, append([]interface{}{CountClients,
		CountPublishers, CountReaders}, args...)...))
}

func (p *program) run() {
	defer close(p.done)

	for _, p := range p.paths {
		p.start()
	}

	checkPathsTicker := time.NewTicker(checkPathPeriod)
	defer checkPathsTicker.Stop()

outer:
	for {
		select {
		case <-checkPathsTicker.C:
			for _, path := range p.paths {
				path.onCheck()
			}

		case conn := <-p.clientNew:
			newClient(p, conn)

		case client := <-p.clientClose:
			if _, ok := p.clients[client]; !ok {
				continue
			}
			client.close()

		case req := <-p.clientDescribe:
			// create path if it doesn't exist
			if _, ok := p.paths[req.pathName]; !ok {
				p.paths[req.pathName] = newPath(p, req.pathName, req.pathConf)
			}

			p.paths[req.pathName].onDescribe(req.client)

		case req := <-p.clientAnnounce:
			// create path if it doesn't exist
			if path, ok := p.paths[req.pathName]; !ok {
				p.paths[req.pathName] = newPath(p, req.pathName, req.pathConf)

			} else {
				if path.source != nil {
					req.res <- fmt.Errorf("someone is already publishing on path '%s'", req.pathName)
					continue
				}
			}

			p.paths[req.pathName].source = req.client
			p.paths[req.pathName].sourceTrackCount = req.trackCount
			p.paths[req.pathName].sourceSdp = req.sdp

			req.client.path = p.paths[req.pathName]
			req.client.state = clientStatePreRecord
			req.res <- nil

		case req := <-p.clientSetupPlay:
			path, ok := p.paths[req.pathName]
			if !ok || !path.sourceReady {
				req.res <- fmt.Errorf("no one is publishing on path '%s'", req.pathName)
				continue
			}

			if req.trackId >= path.sourceTrackCount {
				req.res <- fmt.Errorf("track %d does not exist", req.trackId)
				continue
			}

			req.client.path = path
			req.client.state = clientStatePrePlay
			req.res <- nil

		case client := <-p.clientPlay:
			atomic.AddInt64(p.stats.CountReaders, 1)
			client.state = clientStatePlay
			client.path.readers.Add(client)

		case client := <-p.clientRecord:
			atomic.AddInt64(p.stats.CountPublishers, 1)
			client.state = clientStateRecord

			if client.streamProtocol == gortsplib.StreamProtocolUDP {
				for trackId, track := range client.streamTracks {
					addr := serverudp.MakePublisherAddr(client.ip(), track.rtpPort)
					p.serverUdpRtp.AddPublisher(addr, client, trackId)

					addr = serverudp.MakePublisherAddr(client.ip(), track.rtcpPort)
					p.serverUdpRtcp.AddPublisher(addr, client, trackId)
				}
			}

			client.path.onSourceSetReady()

		case pa := <-p.sourceReady:
			pa.onSourceSetReady()

		case pa := <-p.sourceNotReady:
			pa.onSourceSetNotReady()

		case <-p.terminate:
			break outer
		}
	}

	go func() {
		for {
			select {
			case co, ok := <-p.clientNew:
				if !ok {
					return
				}
				co.Close()

			case <-p.clientClose:
			case <-p.clientDescribe:
			case req := <-p.clientAnnounce:
				req.res <- fmt.Errorf("terminated")
			case req := <-p.clientSetupPlay:
				req.res <- fmt.Errorf("terminated")
			case <-p.clientPlay:
			case <-p.clientRecord:
			case <-p.sourceReady:
			case <-p.sourceNotReady:
			}
		}
	}()

	for _, p := range p.paths {
		p.close()
	}

	p.serverTcp.Close()

	if p.serverUdpRtcp != nil {
		p.serverUdpRtcp.Close()
	}

	if p.serverUdpRtp != nil {
		p.serverUdpRtp.Close()
	}

	for c := range p.clients {
		c.close()
	}

	p.clientsWg.Wait()

	if p.metrics != nil {
		p.metrics.Close()
	}

	if p.pprof != nil {
		p.pprof.Close()
	}

	p.logHandler.Close()

	close(p.clientNew)
	close(p.clientClose)
	close(p.clientDescribe)
	close(p.clientAnnounce)
	close(p.clientSetupPlay)
	close(p.clientPlay)
	close(p.clientRecord)
	close(p.sourceReady)
	close(p.sourceNotReady)
}

func (p *program) close() {
	close(p.terminate)
	<-p.done
}

func main() {
	_, err := newProgram(os.Args[1:])
	if err != nil {
		log.Fatal("ERR: ", err)
	}

	select {}
}
