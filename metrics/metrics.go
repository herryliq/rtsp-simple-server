package metrics

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/aler9/rtsp-simple-server/stats"
)

const (
	address = ":9998"
)

type LogFunc func(string, ...interface{})

type Metrics struct {
	stats    *stats.Stats
	listener net.Listener
	mux      *http.ServeMux
	server   *http.Server
}

func New(log LogFunc, stats *stats.Stats) (*Metrics, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}

	m := &Metrics{
		stats:    stats,
		listener: listener,
	}

	m.mux = http.NewServeMux()
	m.mux.HandleFunc("/metrics", m.onMetrics)

	m.server = &http.Server{
		Handler: m.mux,
	}

	log("[metrics] opened on " + address)

	go m.run()
	return m, nil
}

func (m *Metrics) run() {
	err := m.server.Serve(m.listener)
	if err != http.ErrServerClosed {
		panic(err)
	}
}

func (m *Metrics) Close() {
	m.server.Shutdown(context.Background())
}

func (m *Metrics) onMetrics(w http.ResponseWriter, req *http.Request) {
	now := time.Now().UnixNano() / 1000000

	CountClients := atomic.LoadInt64(m.stats.CountClients)
	CountPublishers := atomic.LoadInt64(m.stats.CountPublishers)
	CountReaders := atomic.LoadInt64(m.stats.CountReaders)
	CountSourcesRtsp := atomic.LoadInt64(m.stats.CountSourcesRtsp)
	CountSourcesRtspRunning := atomic.LoadInt64(m.stats.CountSourcesRtspRunning)
	CountSourcesRtmp := atomic.LoadInt64(m.stats.CountSourcesRtmp)
	CountSourcesRtmpRunning := atomic.LoadInt64(m.stats.CountSourcesRtmpRunning)

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
