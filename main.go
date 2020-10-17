package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"

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

type program struct {
	conf          *conf.Conf
	logHandler    *loghandler.LogHandler
	metrics       *metrics.Metrics
	pprof         *pprof.Pprof
	serverUdpRtp  *serverudp.Server
	serverUdpRtcp *serverudp.Server
	serverTcp     *servertcp.Server
	paths         map[string]*path
	pathsWg       sync.WaitGroup
	clients       map[*client]struct{}
	clientsWg     sync.WaitGroup
	stats         *stats.Stats

	pathClose       chan *path
	clientNew       chan net.Conn
	clientClose     chan *client
	clientDescribe  chan clientDescribeReq
	clientAnnounce  chan clientAnnounceReq
	clientSetupPlay chan clientSetupPlayReq

	terminate chan struct{}
	done      chan struct{}
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
		pathClose:       make(chan *path),
		clientNew:       make(chan net.Conn),
		clientClose:     make(chan *client),
		clientDescribe:  make(chan clientDescribeReq),
		clientAnnounce:  make(chan clientAnnounceReq),
		clientSetupPlay: make(chan clientSetupPlayReq),
		terminate:       make(chan struct{}),
		done:            make(chan struct{}),
	}

	p.logHandler, err = loghandler.New(conf.LogDestinationsParsed, conf.LogFile)
	if err != nil {
		p.closeResources()
		return nil, err
	}

	p.log("rtsp-simple-server %s", Version)

	if conf.Metrics {
		p.metrics, err = metrics.New(p.log, p.stats)
		if err != nil {
			p.closeResources()
			return nil, err
		}
	}

	if conf.Pprof {
		p.pprof, err = pprof.New(p.log)
		if err != nil {
			p.closeResources()
			return nil, err
		}
	}

	if _, ok := conf.ProtocolsParsed[gortsplib.StreamProtocolUDP]; ok {
		p.serverUdpRtp, err = serverudp.New(p.log, p.conf.WriteTimeout, conf.RtpPort, gortsplib.StreamTypeRtp)
		if err != nil {
			p.closeResources()
			return nil, err
		}

		p.serverUdpRtcp, err = serverudp.New(p.log, p.conf.WriteTimeout, conf.RtcpPort, gortsplib.StreamTypeRtcp)
		if err != nil {
			p.closeResources()
			return nil, err
		}
	}

	p.serverTcp, err = servertcp.New(p.log, conf.RtspPort,
		func(c net.Conn) { p.clientNew <- c })
	if err != nil {
		p.closeResources()
		return nil, err
	}

	for name, pathConf := range conf.Paths {
		if pathConf.Regexp == nil {
			pa := newPath(p.pathsWg, p.log, p.stats, p.serverUdpRtp, p.serverUdpRtcp,
				p.conf.ReadTimeout, p.conf.WriteTimeout, name, pathConf,
				func(pa *path) { p.pathClose <- pa })
			p.paths[name] = pa
		}
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

outer:
	for {
		select {
		case pa := <-p.pathClose:
			delete(p.paths, pa.name)
			pa.close()

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
				pa := newPath(p.pathsWg, p.log, p.stats, p.serverUdpRtp, p.serverUdpRtcp,
					p.conf.ReadTimeout, p.conf.WriteTimeout, req.pathName, req.pathConf,
					func(pa *path) { p.pathClose <- pa })
				p.paths[req.pathName] = pa
			}

			p.paths[req.pathName].clientDescribe <- req

		case req := <-p.clientSetupPlay:
			if _, ok := p.paths[req.pathName]; !ok {
				req.res <- fmt.Errorf("no one is publishing on path '%s'", req.pathName)
				continue
			}

			p.paths[req.pathName].clientSetupPlay <- req

		case req := <-p.clientAnnounce:
			// create path if it doesn't exist
			if _, ok := p.paths[req.pathName]; !ok {
				pa := newPath(p.pathsWg, p.log, p.stats, p.serverUdpRtp, p.serverUdpRtcp,
					p.conf.ReadTimeout, p.conf.WriteTimeout, req.pathName, req.pathConf,
					func(pa *path) { p.pathClose <- pa })
				p.paths[req.pathName] = pa
			}

			p.paths[req.pathName].clientAnnounce <- req

		case <-p.terminate:
			break outer
		}
	}

	p.closeResources()
}

func (p *program) closeResources() {
	go func() {
		for {
			select {
			case _, ok := <-p.pathClose:
				if !ok {
					return
				}

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
			}
		}
	}()

	for _, p := range p.paths {
		p.close()
	}
	p.pathsWg.Wait()

	if p.serverTcp != nil {
		p.serverTcp.Close()
	}

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

	if p.logHandler != nil {
		p.logHandler.Close()
	}

	close(p.pathClose)
	close(p.clientNew)
	close(p.clientClose)
	close(p.clientDescribe)
	close(p.clientAnnounce)
	close(p.clientSetupPlay)
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
