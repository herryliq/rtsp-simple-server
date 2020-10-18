package servertcp

import (
	"net"
)

type Parent interface {
	Log(string, ...interface{})
	OnServerTCPConn(net.Conn)
}

type Server struct {
	parent Parent

	listener *net.TCPListener

	done chan struct{}
}

func New(port int, parent Parent) (*Server, error) {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{
		Port: port,
	})
	if err != nil {
		return nil, err
	}

	s := &Server{
		parent:   parent,
		listener: listener,
		done:     make(chan struct{}),
	}

	parent.Log("[TCP server] opened on :%d", port)

	go s.run()
	return s, nil
}

func (s *Server) Close() {
	s.listener.Close()
	<-s.done
}

func (s *Server) run() {
	defer close(s.done)

	for {
		conn, err := s.listener.AcceptTCP()
		if err != nil {
			break
		}

		s.parent.OnServerTCPConn(conn)
	}
}
