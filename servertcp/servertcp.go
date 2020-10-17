package servertcp

import (
	"net"
)

type LogFunc func(string, ...interface{})

type OnNewConnFunc func(net.Conn)

type ServerTCP struct {
	onNewConn OnNewConnFunc
	listener  *net.TCPListener

	done chan struct{}
}

func New(logFunc LogFunc, port int, onNewConn OnNewConnFunc) (*ServerTCP, error) {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{
		Port: port,
	})
	if err != nil {
		return nil, err
	}

	s := &ServerTCP{
		onNewConn: onNewConn,
		listener:  listener,
		done:      make(chan struct{}),
	}

	logFunc("[TCP server] opened on :%d", port)

	go s.run()
	return s, nil
}

func (s *ServerTCP) run() {
	defer close(s.done)

	for {
		conn, err := s.listener.AcceptTCP()
		if err != nil {
			break
		}

		s.onNewConn(conn)
	}
}

func (s *ServerTCP) Close() {
	s.listener.Close()
	<-s.done
}
