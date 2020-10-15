package servertcp

import (
	"net"
)

type LogFunc func(string, ...interface{})

type ServerTCP struct {
	listener *net.TCPListener

	done chan struct{}

	NewClient chan net.Conn
}

func New(logFunc LogFunc, port int) (*ServerTCP, error) {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{
		Port: port,
	})
	if err != nil {
		return nil, err
	}

	s := &ServerTCP{
		listener:  listener,
		done:      make(chan struct{}),
		NewClient: make(chan net.Conn),
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

		s.NewClient <- conn
	}

	close(s.NewClient)
}

func (s *ServerTCP) Close() {
	go func() {
		for range s.NewClient {
		}
	}()
	s.listener.Close()
	<-s.done
}
