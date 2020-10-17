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
	OnUdpFramePublished(int, base.StreamType, []byte)
}

type publisherData struct {
	publisher Publisher
	trackId   int
}

type bufAddrPair struct {
	buf  []byte
	addr *net.UDPAddr
}

type LogFunc func(string, ...interface{})

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

func New(log LogFunc, writeTimeout time.Duration, port int, streamType gortsplib.StreamType) (*Server, error) {
	pc, err := net.ListenUDP("udp", &net.UDPAddr{
		Port: port,
	})
	if err != nil {
		return nil, err
	}

	l := &Server{
		writeTimeout: writeTimeout,
		pc:           pc,
		streamType:   streamType,
		readBuf:      multibuffer.New(2, readBufferSize),
		publishers:   make(map[PublisherAddr]*publisherData),
		writec:       make(chan bufAddrPair),
		done:         make(chan struct{}),
	}

	var label string
	if l.streamType == gortsplib.StreamTypeRtp {
		label = "RTP"
	} else {
		label = "RTCP"
	}
	log("[UDP/"+label+" server] opened on :%d", port)

	go l.run()
	return l, nil
}

func (l *Server) run() {
	defer close(l.done)

	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		for w := range l.writec {
			l.pc.SetWriteDeadline(time.Now().Add(l.writeTimeout))
			l.pc.WriteTo(w.buf, w.addr)
		}
	}()

	for {
		buf := l.readBuf.Next()
		n, addr, err := l.pc.ReadFromUDP(buf)
		if err != nil {
			break
		}

		pub := l.getPublisher(MakePublisherAddr(addr.IP, addr.Port))
		if pub == nil {
			continue
		}

		pub.publisher.OnUdpFramePublished(pub.trackId, l.streamType, buf[:n])
	}

	close(l.writec)
	<-writeDone
}

func (l *Server) Close() {
	l.pc.Close()
	<-l.done
}

func (l *Server) Write(data []byte, addr *net.UDPAddr) {
	l.writec <- bufAddrPair{data, addr}
}

func (l *Server) AddPublisher(addr PublisherAddr, publisher Publisher, trackId int) {
	l.publishersMutex.Lock()
	defer l.publishersMutex.Unlock()

	l.publishers[addr] = &publisherData{
		publisher: publisher,
		trackId:   trackId,
	}
}

func (l *Server) RemovePublisher(addr PublisherAddr) {
	l.publishersMutex.Lock()
	defer l.publishersMutex.Unlock()

	delete(l.publishers, addr)
}

func (l *Server) getPublisher(addr PublisherAddr) *publisherData {
	l.publishersMutex.RLock()
	defer l.publishersMutex.RUnlock()

	el, ok := l.publishers[addr]
	if !ok {
		return nil
	}
	return el
}
