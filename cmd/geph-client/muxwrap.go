package main

import (
	"log"
	"net"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/rlp"
	"github.com/xtaci/smux"
)

type muxWrap struct {
	getSession func() *smux.Session

	lock    sync.Mutex
	session *smux.Session
}

func (sw *muxWrap) fixSess() *smux.Session {
	sw.lock.Lock()
	defer sw.lock.Unlock()
	if sw.session == nil {
		sw.session = sw.getSession()
	}
	return sw.session
}

func (sw *muxWrap) DialCmd(cmds ...string) (conn net.Conn, ok bool) {
	retval := make(chan net.Conn)
	go func() {
	start:
		sess := sw.fixSess()
		timeoutCancel := make(chan bool)
		go func() {
			select {
			case <-time.After(time.Second * 10):
				log.Println(cmds, "timing out, resetting")
				sess.Close()
			case <-timeoutCancel:
			}
		}()
		// markSessionNil marks the session nil only if it hasn't already been changed
		markSessionNil := func() {
			sw.lock.Lock()
			if sw.session == sess {
				sw.session = nil
			}
			sw.lock.Unlock()
		}
		strm, err := sess.OpenStream()
		if err != nil {
			sess.Close()
			log.Println(cmds, "can't open stream, trying again", err)
			close(timeoutCancel)
			markSessionNil()
			goto start
		}
		// dial command
		rlp.Encode(strm, cmds)
		// wait for response
		var connected bool
		err = rlp.Decode(strm, &connected)
		if err != nil {
			sess.Close()
			markSessionNil()
			log.Println(cmds, "can't read response, trying again:", err)
			goto start
		}
		close(timeoutCancel)
		select {
		case retval <- strm:
		default:
			log.Println(cmds, "closing late stream. This is BAAAAAD")
			strm.Close()
		}
	}()
	select {
	case conn = <-retval:
		ok = true
		return
	}
}
