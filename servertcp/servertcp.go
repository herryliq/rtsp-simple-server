package servertcp

import (
	"net"
)

type LogFunc func(string, ...interface{})

type OnNewConnFunc func(net.Conn)

type Server struct {
	onNewConn OnNewConnFunc
	listener  *net.TCPListener

	done chan struct{}
}

func New(log LogFunc, port int, onNewConn OnNewConnFunc) (*Server, error) {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{
		Port: port,
	})
	if err != nil {
		return nil, err
	}

	s := &Server{
		onNewConn: onNewConn,
		listener:  listener,
		done:      make(chan struct{}),
	}

	log("[TCP server] opened on :%d", port)

	go s.run()
	return s, nil
}

func (s *Server) run() {
	defer close(s.done)

	for {
		conn, err := s.listener.AcceptTCP()
		if err != nil {
			break
		}

		s.onNewConn(conn)
	}
}

func (s *Server) Close() {
	s.listener.Close()
	<-s.done
}
