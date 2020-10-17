package pprof

import (
	"context"
	"net"
	"net/http"
	_ "net/http/pprof"
)

const (
	address = ":9999"
)

type LogFunc func(string, ...interface{})

type Pprof struct {
	listener net.Listener
	server   *http.Server
}

func New(log LogFunc) (*Pprof, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}

	pp := &Pprof{
		listener: listener,
	}

	pp.server = &http.Server{
		Handler: http.DefaultServeMux,
	}

	log("[pprof] opened on " + address)

	go pp.run()
	return pp, nil
}

func (pp *Pprof) run() {
	err := pp.server.Serve(pp.listener)
	if err != http.ErrServerClosed {
		panic(err)
	}
}

func (pp *Pprof) Close() {
	pp.server.Shutdown(context.Background())
}
