package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"sync/atomic"

	"github.com/aler9/gortsplib"
	"gopkg.in/alecthomas/kingpin.v2"

	"github.com/aler9/rtsp-simple-server/client"
	"github.com/aler9/rtsp-simple-server/clientman"
	"github.com/aler9/rtsp-simple-server/conf"
	"github.com/aler9/rtsp-simple-server/loghandler"
	"github.com/aler9/rtsp-simple-server/metrics"
	"github.com/aler9/rtsp-simple-server/pathman"
	"github.com/aler9/rtsp-simple-server/pprof"
	"github.com/aler9/rtsp-simple-server/servertcp"
	"github.com/aler9/rtsp-simple-server/serverudp"
	"github.com/aler9/rtsp-simple-server/stats"
)

var Version = "v0.0.0"

type program struct {
	conf          *conf.Conf
	stats         *stats.Stats
	logHandler    *loghandler.LogHandler
	metrics       *metrics.Metrics
	pprof         *pprof.Pprof
	serverUdpRtp  *serverudp.Server
	serverUdpRtcp *serverudp.Server
	serverTcp     *servertcp.Server
	pathMan       *pathman.PathManager
	clientMan     *clientman.ClientManager

	serverTCPConn      chan net.Conn
	pathManClientClose chan *client.Client

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
		conf:               conf,
		serverTCPConn:      make(chan net.Conn),
		pathManClientClose: make(chan *client.Client),
		terminate:          make(chan struct{}),
		done:               make(chan struct{}),
	}

	p.stats = stats.New()

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

	p.pathMan = pathman.New(p.stats, p.serverUdpRtp, p.serverUdpRtcp,
		p.conf.ReadTimeout, p.conf.WriteTimeout, p.conf.AuthMethodsParsed,
		conf.Paths, p)

	p.clientMan = clientman.New(p.stats, p.serverUdpRtp, p.serverUdpRtcp,
		p.conf.ReadTimeout, p.conf.WriteTimeout, p.conf.RunOnConnect,
		p.conf.ProtocolsParsed, p.pathMan, p)

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
		case c := <-p.serverTCPConn:
			p.clientMan.OnClientNew(c)

		case c := <-p.pathManClientClose:
			p.clientMan.OnClientClose(c)

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
			case co, ok := <-p.serverTCPConn:
				if !ok {
					return
				}
				co.Close()

			case <-p.pathManClientClose:
			}
		}
	}()

	if p.clientMan != nil {
		p.clientMan.Close()
	}

	if p.pathMan != nil {
		p.pathMan.Close()
	}

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

	close(p.serverTCPConn)
	close(p.pathManClientClose)
}

func (p *program) close() {
	close(p.terminate)
	<-p.done
}

func (p *program) OnServerTCPConn(conn net.Conn) {
	p.serverTCPConn <- conn
}

func (p *program) OnPathManClientClose(c *client.Client) {
	p.pathManClientClose <- c
}

func main() {
	_, err := newProgram(os.Args[1:])
	if err != nil {
		log.Fatal("ERR: ", err)
	}

	select {}
}
