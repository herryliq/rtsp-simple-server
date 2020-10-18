package serverudp

import (
	"net"
	"sync"
	"time"

	"github.com/aler9/gortsplib"
	"github.com/aler9/gortsplib/base"
	"github.com/aler9/gortsplib/multibuffer"
)

const (
	readBufferSize = 2048
)

type PublisherAddr struct {
	ip   [net.IPv6len]byte // use a fixed-size array to enable the equality operator
	port int
}

func MakePublisherAddr(ip net.IP, port int) PublisherAddr {
	ret := PublisherAddr{
		port: port,
	}

	if len(ip) == net.IPv4len {
		copy(ret.ip[0:], []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff}) // v4InV6Prefix
		copy(ret.ip[12:], ip)
	} else {
		copy(ret.ip[:], ip)
	}

	return ret
}

type Publisher interface {
	OnUdpPublisherFrame(int, base.StreamType, []byte)
}

type publisherData struct {
	publisher Publisher
	trackId   int
}

type bufAddrPair struct {
	buf  []byte
	addr *net.UDPAddr
}

type Parent interface {
	Log(string, ...interface{})
}

type Server struct {
	writeTimeout    time.Duration
	pc              *net.UDPConn
	streamType      gortsplib.StreamType
	readBuf         *multibuffer.MultiBuffer
	publishersMutex sync.RWMutex
	publishers      map[PublisherAddr]*publisherData

	writec chan bufAddrPair
	done   chan struct{}
}

func New(writeTimeout time.Duration, port int,
	streamType gortsplib.StreamType, parent Parent) (*Server, error) {
	pc, err := net.ListenUDP("udp", &net.UDPAddr{
		Port: port,
	})
	if err != nil {
		return nil, err
	}

	s := &Server{
		writeTimeout: writeTimeout,
		pc:           pc,
		streamType:   streamType,
		readBuf:      multibuffer.New(2, readBufferSize),
		publishers:   make(map[PublisherAddr]*publisherData),
		writec:       make(chan bufAddrPair),
		done:         make(chan struct{}),
	}

	var label string
	if s.streamType == gortsplib.StreamTypeRtp {
		label = "RTP"
	} else {
		label = "RTCP"
	}
	parent.Log("[UDP/"+label+" server] opened on :%d", port)

	go s.run()
	return s, nil
}

func (s *Server) run() {
	defer close(s.done)

	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		for w := range s.writec {
			s.pc.SetWriteDeadline(time.Now().Add(s.writeTimeout))
			s.pc.WriteTo(w.buf, w.addr)
		}
	}()

	for {
		buf := s.readBuf.Next()
		n, addr, err := s.pc.ReadFromUDP(buf)
		if err != nil {
			break
		}

		pub := s.getPublisher(MakePublisherAddr(addr.IP, addr.Port))
		if pub == nil {
			continue
		}

		pub.publisher.OnUdpPublisherFrame(pub.trackId, s.streamType, buf[:n])
	}

	close(s.writec)
	<-writeDone
}

func (s *Server) Close() {
	s.pc.Close()
	<-s.done
}

func (s *Server) Port() int {
	return s.pc.LocalAddr().(*net.UDPAddr).Port
}

func (s *Server) Write(data []byte, addr *net.UDPAddr) {
	s.writec <- bufAddrPair{data, addr}
}

func (s *Server) AddPublisher(addr PublisherAddr, publisher Publisher, trackId int) {
	s.publishersMutex.Lock()
	defer s.publishersMutex.Unlock()

	s.publishers[addr] = &publisherData{
		publisher: publisher,
		trackId:   trackId,
	}
}

func (s *Server) RemovePublisher(addr PublisherAddr) {
	s.publishersMutex.Lock()
	defer s.publishersMutex.Unlock()

	delete(s.publishers, addr)
}

func (s *Server) getPublisher(addr PublisherAddr) *publisherData {
	s.publishersMutex.RLock()
	defer s.publishersMutex.RUnlock()

	el, ok := s.publishers[addr]
	if !ok {
		return nil
	}
	return el
}
