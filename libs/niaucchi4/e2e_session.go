package niaucchi4

import (
	"errors"
	"log"
	"math"
	"net"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru"
	"github.com/minio/highwayhash"
	"golang.org/x/time/rate"
)

type e2eSession struct {
	remote       []net.Addr
	info         []e2eLinkInfo
	sessid       SessionAddr
	rdqueue      [][]byte
	dupRateLimit *rate.Limiter
	lastSend     time.Time
	lastRemid    int
	dedup        *lru.Cache

	lock sync.Mutex
}

func newSession(sessid [16]byte) *e2eSession {
	cache, _ := lru.New(128)
	return &e2eSession{
		dupRateLimit: rate.NewLimiter(10, 10),
		dedup:        cache,
		sessid:       sessid,
	}
}

type e2eLinkInfo struct {
	sendsn uint64
	acksn  uint64
	recvsn uint64

	sendTimes [1024]int64
	lastPing  int64
	lastRecv  int64
}

func (el e2eLinkInfo) getScore() float64 {
	now := time.Now().UnixNano() / 1000000
	since := now - el.lastRecv
	return math.Sqrt((float64(since) * math.Max(50, float64(el.lastPing))))
}

type e2ePacket struct {
	Session SessionAddr
	Sn      uint64
	Ack     uint64
	Body    []byte
	Padding []byte
}

// DebugInfo prints out a bunch of debug info.
func (es *e2eSession) DebugInfo() {
	es.lock.Lock()
	defer es.lock.Unlock()
	log.Printf("SESSID = %x", es.sessid[:8])
	for i := range es.remote {
		log.Printf("R %v :: %v / %vms %.2f", es.remote[i], es.info[i].recvsn, es.info[i].lastPing, es.info[i].getScore())
	}
	log.Println("")
}

func (es *e2eSession) AddPath(host net.Addr) {
	es.lock.Lock()
	defer es.lock.Unlock()
	for _, h := range es.remote {
		if h.String() == host.String() {
			return
		}
	}
	if doLogging {
		log.Println("N4: adding new path", host)
	}
	es.remote = append(es.remote, host)
	es.info = append(es.info, e2eLinkInfo{lastPing: 10000000})
}

// Input processes a packet through the e2e session state.
func (es *e2eSession) Input(pkt e2ePacket, source net.Addr) {
	es.lock.Lock()
	defer es.lock.Unlock()
	if pkt.Session != es.sessid {
		log.Println("pkt.Session =", pkt.Session, "; es.sessid =", es.sessid)
		panic("wrong sessid passed to Input")
	}
	// first find the remote
	remid := -1
	for i, v := range es.remote {
		if v.String() == source.String() {
			remid = i
			break
		}
	}
	if remid < 0 {
		if doLogging {
			log.Println("N4: e2eSession.Input() failed to find remid")
		}
		return
	}
	// parse the stuff
	if pkt.Sn < es.info[remid].recvsn {
	} else {
		es.info[remid].recvsn = pkt.Sn
		es.info[remid].acksn = pkt.Ack
		es.info[remid].lastRecv = time.Now().UnixNano() / 1000000
		now := time.Now().UnixNano() / 1000000
		sentTime := es.info[remid].sendTimes[pkt.Ack%1024]
		ping := now - sentTime
		if ping < 1000 {
			es.info[remid].lastPing = (es.info[remid].lastPing*9 + ping*1) / 10
		}
	}
	bodyHash := highwayhash.Sum128(pkt.Body, make([]byte, 32))
	if es.dedup.Contains(bodyHash) {
	} else {
		es.dedup.Add(bodyHash, true)
		es.rdqueue = append(es.rdqueue, pkt.Body)
	}
}

// Send sends a packet. It returns instructions to where the packet should be sent etc
func (es *e2eSession) Send(payload []byte, sendCallback func(e2ePacket, net.Addr)) (err error) {
	es.lock.Lock()
	defer es.lock.Unlock()
	send := func(remid int) {
		// create pkt
		toSend := e2ePacket{
			Session: es.sessid,
			Sn:      es.info[remid].sendsn,
			Ack:     es.info[remid].recvsn + 1,
			Body:    payload,
		}
		es.info[remid].sendTimes[(toSend.Sn+1)%1024] = time.Now().UnixNano() / 1000000
		es.info[remid].sendsn++
		dest := es.remote[remid]
		sendCallback(toSend, dest)
	}
	now := time.Now()
	// find the right destination
	if es.dupRateLimit.Allow() {
		for remid := range es.remote {
			send(remid)
		}
	} else {
		remid := -1
		if now.Sub(es.lastSend).Milliseconds() > 500 {
			lowPoint := 1e20
			for i, li := range es.info {
				if score := li.getScore(); score < lowPoint {
					lowPoint = score
					remid = i
				}
			}
			if remid == -1 {
				err = errors.New("cannot find any path")
				return
			}
		} else {
			remid = es.lastRemid
		}
		if es.lastRemid != remid && doLogging {
			log.Println("N4: changing path to", es.remote[remid])
			go es.DebugInfo()
		}
		es.lastRemid = remid
		send(remid)
		es.lastSend = now
	}
	return
}

// FlushReadQueue flushes the entire read queue.
func (es *e2eSession) FlushReadQueue(onPacket func([]byte)) {
	es.lock.Lock()
	defer es.lock.Unlock()
	for _, b := range es.rdqueue {
		onPacket(b)
	}
	es.rdqueue = es.rdqueue[:0]
}
