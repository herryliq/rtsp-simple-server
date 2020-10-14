package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

const (
	metricsAddress = ":9998"
)

type metrics struct {
	p        *program
	listener net.Listener
	mux      *http.ServeMux
	server   *http.Server
}

func newMetrics(p *program) (*metrics, error) {
	listener, err := net.Listen("tcp", metricsAddress)
	if err != nil {
		return nil, err
	}

	m := &metrics{
		p:        p,
		listener: listener,
	}

	m.mux = http.NewServeMux()
	m.mux.HandleFunc("/metrics", m.onMetrics)

	m.server = &http.Server{
		Handler: m.mux,
	}

	m.p.log("[metrics] opened on " + metricsAddress)
	return m, nil
}

func (m *metrics) run() {
	err := m.server.Serve(m.listener)
	if err != http.ErrServerClosed {
		panic(err)
	}
}

func (m *metrics) close() {
	m.server.Shutdown(context.Background())
}

func (m *metrics) onMetrics(w http.ResponseWriter, req *http.Request) {
	now := time.Now().UnixNano() / 1000000

	CountClients := atomic.LoadInt64(m.p.stats.CountClients)
	CountPublishers := atomic.LoadInt64(m.p.stats.CountPublishers)
	CountReaders := atomic.LoadInt64(m.p.stats.CountReaders)
	CountSourcesRtsp := atomic.LoadInt64(m.p.stats.CountSourcesRtsp)
	CountSourcesRtspRunning := atomic.LoadInt64(m.p.stats.CountSourcesRtspRunning)
	CountSourcesRtmp := atomic.LoadInt64(m.p.stats.CountSourcesRtmp)
	CountSourcesRtmpRunning := atomic.LoadInt64(m.p.stats.CountSourcesRtmpRunning)

	out := ""
	out += fmt.Sprintf("rtsp_clients{state=\"idle\"} %d %v\n",
		CountClients-CountPublishers-CountReaders, now)
	out += fmt.Sprintf("rtsp_clients{state=\"publishing\"} %d %v\n",
		CountPublishers, now)
	out += fmt.Sprintf("rtsp_clients{state=\"reading\"} %d %v\n",
		CountReaders, now)
	out += fmt.Sprintf("rtsp_sources{type=\"rtsp\",state=\"idle\"} %d %v\n",
		CountSourcesRtsp-CountSourcesRtspRunning, now)
	out += fmt.Sprintf("rtsp_sources{type=\"rtsp\",state=\"running\"} %d %v\n",
		CountSourcesRtspRunning, now)
	out += fmt.Sprintf("rtsp_sources{type=\"rtmp\",state=\"idle\"} %d %v\n",
		CountSourcesRtmp-CountSourcesRtmpRunning, now)
	out += fmt.Sprintf("rtsp_sources{type=\"rtmp\",state=\"running\"} %d %v\n",
		CountSourcesRtmpRunning, now)

	w.WriteHeader(http.StatusOK)
	io.WriteString(w, out)
}
