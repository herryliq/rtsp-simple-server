package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"

	"github.com/aler9/gortsplib"
	"github.com/aler9/gortsplib/base"
	"gopkg.in/alecthomas/kingpin.v2"

	"github.com/aler9/rtsp-simple-server/client"
	"github.com/aler9/rtsp-simple-server/conf"
	"github.com/aler9/rtsp-simple-server/loghandler"
	"github.com/aler9/rtsp-simple-server/metrics"
	"github.com/aler9/rtsp-simple-server/path"
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
	paths         map[string]*path.Path
	pathsWg       sync.WaitGroup
	clients       map[*client.Client]struct{}
	clientsWg     sync.WaitGroup
	stats         *stats.Stats

	pathClose       chan *path.Path
	clientNew       chan net.Conn
	clientClose     chan *client.Client
	clientDescribe  chan path.ClientDescribeReq
	clientAnnounce  chan path.ClientAnnounceReq
	clientSetupPlay chan path.ClientSetupPlayReq

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
		paths:           make(map[string]*path.Path),
		clients:         make(map[*client.Client]struct{}),
		stats:           stats.New(),
		pathClose:       make(chan *path.Path),
		clientNew:       make(chan net.Conn),
		clientClose:     make(chan *client.Client),
		clientDescribe:  make(chan path.ClientDescribeReq),
		clientAnnounce:  make(chan path.ClientAnnounceReq),
		clientSetupPlay: make(chan path.ClientSetupPlayReq),
		terminate:       make(chan struct{}),
		done:            make(chan struct{}),
	}

	p.logHandler, err = loghandler.New(conf.LogDestinationsParsed, conf.LogFile)
	if err != nil {
		p.closeResources()
		return nil, err
	}

	p.Log("rtsp-simple-server %s", Version)

	if conf.Metrics {
		p.metrics, err = metrics.New(p.stats, p)
		if err != nil {
			p.closeResources()
			return nil, err
		}
	}

	if conf.Pprof {
		p.pprof, err = pprof.New(p)
		if err != nil {
			p.closeResources()
			return nil, err
		}
	}

	if _, ok := conf.ProtocolsParsed[gortsplib.StreamProtocolUDP]; ok {
		p.serverUdpRtp, err = serverudp.New(p.conf.WriteTimeout, conf.RtpPort,
			gortsplib.StreamTypeRtp, p)
		if err != nil {
			p.closeResources()
			return nil, err
		}

		p.serverUdpRtcp, err = serverudp.New(p.conf.WriteTimeout, conf.RtcpPort,
			gortsplib.StreamTypeRtcp, p)
		if err != nil {
			p.closeResources()
			return nil, err
		}
	}

	p.serverTcp, err = servertcp.New(conf.RtspPort, p)
	if err != nil {
		p.closeResources()
		return nil, err
	}

	for name, pathConf := range conf.Paths {
		if pathConf.Regexp == nil {
			pa := path.New(&p.pathsWg, p.stats, p.serverUdpRtp, p.serverUdpRtcp,
				p.conf.ReadTimeout, p.conf.WriteTimeout, name, pathConf, p)
			p.paths[name] = pa
		}
	}

	go p.run()

	return p, nil
}

func (p *program) Log(format string, args ...interface{}) {
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
			p.onPathClose(pa)

		case conn := <-p.clientNew:
			c := client.New(&p.clientsWg,
				p.stats,
				p.serverUdpRtp,
				p.serverUdpRtcp,
				p.conf.ReadTimeout,
				p.conf.WriteTimeout,
				p.conf.RunOnConnect,
				p.conf.ProtocolsParsed,
				conn,
				p)
			p.clients[c] = struct{}{}

		case c := <-p.clientClose:
			if _, ok := p.clients[c]; !ok {
				continue
			}
			p.onClientClose(c)

		case req := <-p.clientDescribe:
			pathConf, err := p.conf.CheckPathNameAndFindConf(req.PathName)
			if err != nil {
				req.Res <- path.ClientDescribeRes{nil, err}
				continue
			}

			err = req.Client.Authenticate(p.conf.AuthMethodsParsed, pathConf.ReadIpsParsed,
				pathConf.ReadUser, pathConf.ReadPass, req.Req)
			if err != nil {
				req.Res <- path.ClientDescribeRes{nil, err}
				continue
			}

			// create path if it doesn't exist
			if _, ok := p.paths[req.PathName]; !ok {
				pa := path.New(&p.pathsWg, p.stats, p.serverUdpRtp, p.serverUdpRtcp,
					p.conf.ReadTimeout, p.conf.WriteTimeout, req.PathName, pathConf, p)
				p.paths[req.PathName] = pa
			}

			p.paths[req.PathName].OnProgramClientDescribe(req)

		case req := <-p.clientAnnounce:
			pathConf, err := p.conf.CheckPathNameAndFindConf(req.PathName)
			if err != nil {
				req.Res <- path.ClientAnnounceRes{nil, err}
				continue
			}

			err = req.Client.Authenticate(p.conf.AuthMethodsParsed,
				pathConf.PublishIpsParsed, pathConf.PublishUser, pathConf.PublishPass, req.Req)
			if err != nil {
				req.Res <- path.ClientAnnounceRes{nil, err}
				continue
			}

			// create path if it doesn't exist
			if _, ok := p.paths[req.PathName]; !ok {
				pa := path.New(&p.pathsWg, p.stats, p.serverUdpRtp, p.serverUdpRtcp,
					p.conf.ReadTimeout, p.conf.WriteTimeout, req.PathName, pathConf, p)
				p.paths[req.PathName] = pa
			}

			p.paths[req.PathName].OnProgramClientAnnounce(req)

		case req := <-p.clientSetupPlay:
			if _, ok := p.paths[req.PathName]; !ok {
				req.Res <- path.ClientSetupPlayRes{nil, fmt.Errorf("no one is publishing on path '%s'", req.PathName)}
				continue
			}

			pathConf, err := p.conf.CheckPathNameAndFindConf(req.PathName)
			if err != nil {
				req.Res <- path.ClientSetupPlayRes{nil, err}
				continue
			}

			err = req.Client.Authenticate(p.conf.AuthMethodsParsed,
				pathConf.ReadIpsParsed, pathConf.ReadUser, pathConf.ReadPass, req.Req)
			if err != nil {
				req.Res <- path.ClientSetupPlayRes{nil, err}
				continue
			}

			p.paths[req.PathName].OnProgramClientSetupPlay(req)

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

			case req := <-p.clientDescribe:
				req.Res <- path.ClientDescribeRes{nil, fmt.Errorf("terminated")}

			case req := <-p.clientAnnounce:
				req.Res <- path.ClientAnnounceRes{nil, fmt.Errorf("terminated")}

			case req := <-p.clientSetupPlay:
				req.Res <- path.ClientSetupPlayRes{nil, fmt.Errorf("terminated")}
			}
		}
	}()

	for c := range p.clients {
		p.onClientClose(c)
	}
	p.clientsWg.Wait()

	for _, pa := range p.paths {
		p.onPathClose(pa)
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

func (p *program) onPathClose(pa *path.Path) {
	delete(p.paths, pa.Name())
	pa.Close()
}

func (p *program) onClientClose(c *client.Client) {
	delete(p.clients, c)
	c.Close()
}

func (p *program) OnServerTCPConn(conn net.Conn) {
	p.clientNew <- conn
}

func (p *program) OnPathClose(pa *path.Path) {
	p.pathClose <- pa
}

func (p *program) OnPathClientClose(c *client.Client) {
	p.clientClose <- c
}

func (p *program) OnClientClose(c *client.Client) {
	p.clientClose <- c
}

func (p *program) OnClientDescribe(c *client.Client, pathName string, req *base.Request) (client.Path, error) {
	res := make(chan path.ClientDescribeRes)
	p.clientDescribe <- path.ClientDescribeReq{res, c, pathName, req}
	re := <-res
	return re.Path, re.Err
}

func (p *program) OnClientAnnounce(c *client.Client, pathName string, tracks gortsplib.Tracks, req *base.Request) (client.Path, error) {
	res := make(chan path.ClientAnnounceRes)
	p.clientAnnounce <- path.ClientAnnounceReq{res, c, pathName, tracks, req}
	re := <-res
	return re.Path, re.Err
}

func (p *program) OnClientSetupPlay(c *client.Client, basePath string, trackId int, req *base.Request) (client.Path, error) {
	res := make(chan path.ClientSetupPlayRes)
	p.clientSetupPlay <- path.ClientSetupPlayReq{res, c, basePath, trackId, req}
	re := <-res
	return re.Path, re.Err
}

func main() {
	_, err := newProgram(os.Args[1:])
	if err != nil {
		log.Fatal("ERR: ", err)
	}

	select {}
}
